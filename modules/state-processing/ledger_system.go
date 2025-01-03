package stateEngine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

//Implementation notes:
//The ledger system should operate using a real oplog and virtual ledger
//For example, a deposit record has no oplog, but a virtual ledger op indicating deposit success
//Where as a transfer has an oplog signaling intent, which can be parsed into correct ledger ops to update balance.

// Transaction execution process:
// - Inclusion (hive or vsc)
// - Execution which produces (oplog)
// - Ledger update (virtual ledger)
// In summary:
// 1. TX execution -> 2. Oplog -> 3. Ledger update (locally calculated value)

var assetTypes = []string{"HIVE", "HBD"}

const ETH_REGEX = "^0x[a-fA-F0-9]{40}$"
const HIVE_REGEX = `^[a-z][0-9a-z\-]*[0-9a-z](\.[a-z][0-9a-z\-]*[0-9a-z])*$`

type LedgerUpdate struct {
	Id string
	//Block Index
	BlockHeight int64
	//Block Index: Index of the TX in the block
	BIdx int64
	//Op Index: Index of the operation in the TX
	OpIdx int64

	Owner  string
	Amount int64
	Asset  string
	Memo   string
	//transfer, withdraw, stake, unstake
	Type string
}

type OpLogEvent struct {
	To     string
	From   string
	Amount int64
	Asset  string
	Memo   string
	Type   string

	//Not parted of compiled state
	Id    string
	BIdx  int64
	OpIdx int64

	//Fee for instant stake unstake
	// Fee int64 `json:"fee,omitempty"`
	Params map[string]interface{} `json:"params,omitempty"`
}

type LedgerResult struct {
	Ok  bool
	Msg string
}

type LedgerSystem struct {
	BalanceDb ledgerDb.Balances
	LedgerDb  ledgerDb.Ledger

	//Bridge actions are side effects that require secondary (outside of execution) processing to complete
	//Some examples are withdrawals, and stake/unstake operations. Other future operations might be applicable as well
	//Anything that requires on chain processing to complete
	ActionsDb ledgerDb.BridgeActions

	GatewayLedgerDb ledgerDb.GatewayLedger
}

// func (ls *LedgerSystem) ApplyOp(op LedgerOp) {

// }

func (ls *LedgerSystem) GetBalance(account string, blockHeight int64, asset string) int64 {
	if !slices.Contains(assetTypes, asset) {
		return 0
	}
	ledgerResults, _ := ls.LedgerDb.GetLedgerAfterHeight(account, blockHeight, asset, nil)
	balRecord, _ := ls.BalanceDb.GetBalanceRecord(account, blockHeight, asset)

	for _, v := range *ledgerResults {
		balRecord = balRecord + v.Amount
	}
	return balRecord
}

// Used during live execution of transfers such as those from contracts or user transaction
type LedgerExecutor struct {
	Ls *LedgerSystem

	//List of finalized operations such as transfers, withdrawals.
	//Expected to be identical when block is produced and used during block creation
	Oplog []OpLogEvent
	//Virtual ledger is a cache of all balance changes (virtual and non-virtual)
	//Includes deposits, transfers (in-n-out), withdrawals, and stake/unstake operations (future)
	VirtualLedger map[string][]LedgerUpdate
	BlockHeight   int64
}

func (le *LedgerExecutor) SetHeight(bh int64) {
	le.BlockHeight = bh
}

// Pretends blocks are being produced; utility function
func (le *LedgerExecutor) Mine(i int64) {
	le.BlockHeight += i
}

// Empties the virtual state, such as when a block is executed
func (le *LedgerExecutor) Flush() {
	le.VirtualLedger = make(map[string][]LedgerUpdate)
}

func (le *LedgerExecutor) ExecuteTransfer(OpLogEvent OpLogEvent) LedgerResult {
	//Check if the from account has enough balance

	if OpLogEvent.Amount <= 0 {
		return LedgerResult{
			Ok:  false,
			Msg: "Invalid amount",
		}
	}
	if OpLogEvent.To == OpLogEvent.From {
		return LedgerResult{
			Ok:  false,
			Msg: "Cannot send to self",
		}
	}
	if !slices.Contains(assetTypes, OpLogEvent.Asset) {
		return LedgerResult{
			Ok:  false,
			Msg: "Invalid asset",
		}
	}
	// fromBal := le.Ls.GetBalance(OpLogEvent.From, le.BlockHeight, OpLogEvent.From)
	fromBal := le.SnapshotForAccount(OpLogEvent.From, le.BlockHeight, OpLogEvent.Asset)
	fmt.Println("fromBal", fromBal)
	if fromBal < OpLogEvent.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "Insufficient balance",
		}
	}

	le.Oplog = append(le.Oplog, OpLogEvent)

	le.VirtualLedger[OpLogEvent.From] = append(le.VirtualLedger[OpLogEvent.From], LedgerUpdate{
		Id:    OpLogEvent.Id,
		BIdx:  OpLogEvent.BIdx,
		OpIdx: OpLogEvent.OpIdx,

		Owner:  OpLogEvent.From,
		Amount: -OpLogEvent.Amount,
		Asset:  OpLogEvent.Asset,
		Type:   "transfer",
		Memo:   OpLogEvent.Memo,
	})
	le.VirtualLedger[OpLogEvent.To] = append(le.VirtualLedger[OpLogEvent.To], LedgerUpdate{
		Id:    OpLogEvent.Id,
		OpIdx: OpLogEvent.OpIdx,
		BIdx:  OpLogEvent.BIdx,

		Owner:  OpLogEvent.To,
		Amount: OpLogEvent.Amount,
		Asset:  OpLogEvent.Asset,
		Type:   "transfer",
		Memo:   OpLogEvent.Memo,
	})

	return LedgerResult{
		Ok:  true,
		Msg: "Success",
	}
}

type Deposit struct {
	Id    string
	OpIdx int64
	BIdx  int64

	Account string
	Amount  int64
	Asset   string
	Memo    string
	From    string
}

// Add more fields as necessary
type DepositParams struct {
	To string `json:"to"`
}

func (le *LedgerExecutor) Deposit(deposit Deposit) {
	decodedParams := DepositParams{}
	values, err := url.ParseQuery(deposit.Memo)
	if err == nil {
		decodedParams.To = values.Get("to")
	} else {
		err = json.Unmarshal([]byte(deposit.Memo), &decodedParams)
		if err != nil {
			decodedParams.To = deposit.From
		}
	}

	matchedHive, _ := regexp.MatchString(HIVE_REGEX, decodedParams.To)
	matchedEth, _ := regexp.MatchString(ETH_REGEX, decodedParams.To)

	if matchedHive && len(decodedParams.To) >= 3 && len(decodedParams.To) < 17 {
		decodedParams.To = `hive:` + decodedParams.To
	} else if matchedEth {
		decodedParams.To = `did:pkh:eip155:1:` + decodedParams.To
	} else if strings.HasPrefix(decodedParams.To, "hive:") {
		//No nothing. It's parsed correctly
		matchedEth, _ := regexp.MatchString(HIVE_REGEX, strings.Split(decodedParams.To, ":")[1])
		if !(matchedEth && len(decodedParams.To) >= 3 && len(decodedParams.To) < 17) {
			decodedParams.To = "hive:" + deposit.From
		}
	} else {
		//Default to the original sender to prevent fund loss
		// addr, _ := NormalizeAddress(deposit.From, "hive")
		decodedParams.To = deposit.From
	}
	if le.VirtualLedger == nil {
		le.VirtualLedger = make(map[string][]LedgerUpdate)
	}

	ledgerUpdate := LedgerUpdate{
		Id:    deposit.Id,
		BIdx:  deposit.BIdx,
		OpIdx: deposit.OpIdx,

		Owner:  decodedParams.To,
		Amount: deposit.Amount,
		Asset:  deposit.Asset,
		Type:   "deposit",
	}
	le.VirtualLedger[decodedParams.To] = append(le.VirtualLedger[decodedParams.To], ledgerUpdate)
	le.Ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id:          deposit.Id,
		BlockHeight: le.BlockHeight,
		Amount:      deposit.Amount,
		Asset:       deposit.Asset,
		From:        deposit.From,
		Owner:       decodedParams.To,
		Type:        "deposit",
		TxId:        deposit.Id,
	})
}

func (le *LedgerExecutor) IndexWithdraws(withdrawIds []string) {

}

type WithdrawParams struct {
	Id     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Asset  string `json:"asset"`
	Amount int64  `json:"amount"`
	Memo   string `json:"memo"`

	BIdx  int64 `json:"bidx"`
	OpIdx int64 `json:"opidx"`
}

func (le *LedgerExecutor) Withdraw(withdraw WithdrawParams) LedgerResult {
	if withdraw.Amount <= 0 {
		return LedgerResult{
			Ok:  false,
			Msg: "Invalid amount",
		}
	}

	if !slices.Contains(assetTypes, withdraw.Asset) {
		return LedgerResult{
			Ok:  false,
			Msg: "Invalid asset",
		}
	}

	var dest string
	matchedHive, _ := regexp.MatchString(HIVE_REGEX, withdraw.To)

	if matchedHive && len(withdraw.To) >= 3 && len(withdraw.To) < 17 {
		dest = `hive:` + withdraw.To
	} else if strings.HasPrefix(withdraw.To, "hive:") {
		//No nothing. It's parsed correctly
		splitHive := strings.Split(withdraw.To, ":")[1]
		matchedHive, _ := regexp.MatchString(HIVE_REGEX, splitHive)
		if matchedHive && len(splitHive) >= 3 && len(splitHive) < 17 {
			dest = withdraw.To
		} else {
			return LedgerResult{
				Ok:  false,
				Msg: "Invalid destination",
			}
		}
	} else {
		return LedgerResult{
			Ok:  false,
			Msg: "Invalid destination",
		}
	}

	balAmt := le.SnapshotForAccount(withdraw.From, le.BlockHeight, withdraw.Asset)

	if balAmt < withdraw.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "Insufficient balance",
		}
	}

	le.VirtualLedger[withdraw.From] = append(le.VirtualLedger[withdraw.From], LedgerUpdate{
		Id:    withdraw.Id,
		BIdx:  withdraw.BIdx,
		OpIdx: withdraw.OpIdx,

		Owner:  withdraw.From,
		Amount: -withdraw.Amount,
		Asset:  withdraw.Asset,
		Type:   "withdraw",
		Memo:   withdraw.Memo,
	})

	le.Oplog = append(le.Oplog, OpLogEvent{
		Id:     withdraw.Id,
		From:   withdraw.From,
		To:     dest,
		Amount: withdraw.Amount,
		Asset:  withdraw.Asset,
		Memo:   withdraw.Memo,
		Type:   "withdraw",

		BIdx:  withdraw.BIdx,
		OpIdx: withdraw.OpIdx,
	})

	return LedgerResult{
		Ok:  true,
		Msg: "Success",
	}
}

func (le *LedgerExecutor) CalculationFractStats(accountList []string, blockHeight int64) struct {
	//Total staked balance of hbd_savings accounts
	StakedBalance int64

	//Total fractional balance; must be staked
	FractionalBalance int64

	//Both numbers should add together to be equivalent to the total hbd savings of the vsc gateway wallet
} {

	//TODO: Make this work without needing to supply list of accounts.
	//Instead pull all accounts above >0 balance

	hbdMap := make(map[string]int64)
	hbdSavingsMap := make(map[string]int64)

	for _, account := range accountList {
		hbdBal := le.SnapshotForAccount(account, blockHeight, "hbd")
		hbdSavingsBal := le.SnapshotForAccount(account, blockHeight, "hbd_savings")
		hbdMap[account] = hbdBal
		hbdSavingsMap[account] = hbdSavingsBal
	}

	topBalances := make([]int64, 0)

	for _, v := range hbdMap {
		topBalances = append(topBalances, v)
	}
	sort.Slice(topBalances, func(i, j int) bool {
		return topBalances[i] > topBalances[j]
	})

	cutOff := 3
	topBals := topBalances[:cutOff]
	belowBals := topBalances[cutOff:]

	topBal := int64(0)
	belowBal := int64(0)

	for _, v := range topBals {
		topBal = topBal + v
	}

	for _, v := range belowBals {
		belowBal = belowBal + v
	}

	fmt.Println("Top Balances", topBalances)
	fmt.Println("Top 5", topBal, "Below 5", belowBal)
	stakedAmt := belowBal / 3
	fmt.Println("StakedAmt", stakedAmt)

	StakedBalance := int64(0)

	for _, v := range hbdSavingsMap {
		StakedBalance = StakedBalance + v
	}

	return struct {
		StakedBalance     int64
		FractionalBalance int64
	}{
		StakedBalance:     StakedBalance,
		FractionalBalance: stakedAmt,
	}
}

func (le *LedgerExecutor) SavingsSync() {

}

type CompiledResult struct {
	OpLog []OpLogEvent
}

func (le *LedgerExecutor) Compile() *CompiledResult {

	return nil
}

type StakeOp struct {
	OpLogEvent
	Instant bool
}

const HBD_UNSTAKE_BLOCKS = int64(86400)

// HBD instant stake/unstake fee
const HBD_INSTANT_FEE = int64(1) // 1%
const HBD_INSTANT_MIN = int64(1) // 0.001 HBD
const HBD_FEE_RECEIVER = "vsc.dao"

// Reserved for the future.
// Stake would trigger an indent to stake funds (immediately removing balance)
// Then trigger a delayed (actual stake) even when the onchain operation is executed through the gateway
// A two part Virtual Ledger operation operating out of sync
func (le *LedgerExecutor) Stake(stakeOp StakeOp) LedgerResult {
	//Cannot stake less than 0.002 HBD
	//As there is a 0.001 HBD fee for instant staking
	if stakeOp.Amount <= 0 || (stakeOp.Instant && stakeOp.Amount < 2) {
		return LedgerResult{
			Ok:  false,
			Msg: "Invalid amount",
		}
	}

	if stakeOp.Asset != "hbd" {
		return LedgerResult{
			Ok:  false,
			Msg: "Invalid asset",
		}
	}

	le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], LedgerUpdate{
		Id:     stakeOp.Id,
		OpIdx:  0,
		Owner:  stakeOp.From,
		Amount: -stakeOp.Amount,
		Asset:  stakeOp.Asset,
		Type:   "stake",
		Memo:   stakeOp.Memo,
	})

	//VSC:
	// - BlockHeight
	// - BkIndex
	// - OpIndex
	// - LIdx

	fee := int64(0)

	if stakeOp.Instant {
		fee = stakeOp.Amount * HBD_INSTANT_FEE / 100
		if fee == 0 {
			//Minimum of
			fee = HBD_INSTANT_MIN
		}
		withdrawAmount := stakeOp.Amount - fee

		le.VirtualLedger[HBD_FEE_RECEIVER] = append(le.VirtualLedger[HBD_FEE_RECEIVER], LedgerUpdate{
			Id:     stakeOp.Id,
			OpIdx:  0,
			Owner:  HBD_FEE_RECEIVER,
			Amount: fee,
			Asset:  "hbd",
			Type:   "fee",
			Memo:   "HBD instant fee",
		})
		le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], LedgerUpdate{
			Id:          stakeOp.Id,
			BlockHeight: le.BlockHeight,
			OpIdx:       0,
			Owner:       stakeOp.To,
			Amount:      withdrawAmount,
			Asset:       "hbd_savings",
			Type:        "stake",
			Memo:        stakeOp.Memo,
		})
	}

	le.Oplog = append(le.Oplog, OpLogEvent{
		Id: stakeOp.Id + "-1",
		// Index: 1,

		From:   stakeOp.From,
		Asset:  stakeOp.Asset,
		Amount: stakeOp.Amount,
		Memo:   stakeOp.Memo,
		Type:   "stake",

		Params: map[string]interface{}{
			"instant": stakeOp.Instant,
			"fee":     fee,
		},
	})

	return LedgerResult{
		Ok:  true,
		Msg: "Success",
	}
}

func (le *LedgerExecutor) Unstake(stakeOp StakeOp) {

}
func (le *LedgerExecutor) SnapshotForAccount(account string, blockHeight int64, asset string) int64 {

	bal, err := le.Ls.BalanceDb.GetBalanceRecord(account, blockHeight, asset)
	if err != nil {
		panic(err)
	}
	for _, v := range le.VirtualLedger[account] {
		//Must be ledger ops with height below or equal to the current block height
		//Current block height ledger ops are recently executed
		if v.BlockHeight <= blockHeight {
			if v.Asset == asset {
				bal += v.Amount
			}
		}
	}
	return bal
}

func (le *LedgerExecutor) Snapshot(RelevantAccounts *[]string) {
	if RelevantAccounts != nil {

	}
}

type FilterLedgerParams struct {
	//Allow all transactions in all blocks up to this height
	FinalHeight int64
	BelowBIdx   int64
	BelowOpIdx  int64
}

// In memory filter for ledger ops for querying virtualOps cache
// Use this for writing safer queries against in memory stack
func FilterLedgerOps(query FilterLedgerParams, array []LedgerUpdate) []LedgerUpdate {
	outArray := make([]LedgerUpdate, 0)
	for _, v := range array {
		allowed := false

		if v.BlockHeight < query.FinalHeight {
			allowed = true
		} else if v.BlockHeight == query.FinalHeight {
			if v.BIdx < query.BelowBIdx {
				if v.OpIdx < query.BelowOpIdx {
					allowed = true
				}
			}
		}

		if allowed {
			outArray = append(outArray, v)
		}
	}

	return outArray
}
