package stateEngine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
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

var assetTypes = []string{"hive", "hbd", "hbd_savings"}

const ETH_REGEX = "^0x[a-fA-F0-9]{40}$"
const HIVE_REGEX = `^[a-z][0-9a-z\-]*[0-9a-z](\.[a-z][0-9a-z\-]*[0-9a-z])*$`

type LedgerUpdate struct {
	Id string
	//Block Index
	BlockHeight uint64
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
	Id          string
	BIdx        int64
	OpIdx       int64
	BlockHeight uint64 `json:"-"`

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

func (ls *LedgerSystem) GetBalance(account string, blockHeight uint64, asset string) int64 {
	if !slices.Contains(assetTypes, asset) {
		return 0
	}

	// TODO: handle errors
	balRecord, lastHeight, _ := ls.BalanceDb.GetBalanceRecord(account, blockHeight, asset)

	ledgerResults, _ := ls.LedgerDb.GetLedgerRange(account, lastHeight, blockHeight, asset)

	fmt.Println("ledgerResults.len()", len(*ledgerResults))
	for _, v := range *ledgerResults {
		balRecord = balRecord + v.Amount
	}
	return balRecord
}

func (ls *LedgerSystem) ClaimHBDInterest(lastClaim uint64, blockHeight uint64, amount int64) {
	//Do distribution of HBD interest on an going forward basis
	//Save to ledger DB the difference.
	ledgerBalances := ls.BalanceDb.GetAll(blockHeight)

	processedBalRecords := make([]ledgerDb.BalanceRecord, 0)
	totalAvg := int64(0)
	//Ensure averages have been updated before distribution;
	for _, balance := range ledgerBalances {
		moreAvg := balance.HBD_SAVINGS * int64(balance.HBD_MODIFY_HEIGHT) / int64(lastClaim)
		fmt.Println("INCREASING TOTAL AVG", balance.HBD_SAVINGS, moreAvg)
		balance.HBD_AVG = balance.HBD_AVG + moreAvg

		processedBalRecords = append(processedBalRecords, balance)
		totalAvg = totalAvg + balance.HBD_AVG
	}

	bsj, _ := json.Marshal(processedBalRecords)

	fmt.Println("Processed bal records", processedBalRecords, string(bsj))

	for id, balance := range processedBalRecords {
		if balance.HBD_AVG == 0 {
			continue
		}

		distributeAmt := balance.HBD_AVG * amount / totalAvg

		fmt.Println("Distribute", distributeAmt)
		if distributeAmt > 0 {
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:          "hbd_interest_" + strconv.Itoa(int(blockHeight)) + "_" + strconv.Itoa(id),
				BlockHeight: blockHeight,
				Amount:      int64(distributeAmt),
				Asset:       "hbd_savings",
				Owner:       balance.Account,
				Type:        "hbd_interest",
			})
		}
	}
	//DONE
}

func (ls *LedgerSystem) ExecuteOplog(oplog []OpLogEvent, startHeight uint64, endBlock uint64) {
	affectedAccounts := map[string]bool{}

	for _, v := range oplog {
		if v.Type == "transfer" {
			affectedAccounts[v.From] = true
			affectedAccounts[v.To] = true
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:          v.Id,
				BlockHeight: endBlock,
				Amount:      -v.Amount,
				Asset:       v.Asset,
				Owner:       v.From,
				Type:        "transfer",
				TxId:        v.Id,
			})
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:          v.Id,
				BlockHeight: endBlock,
				Amount:      v.Amount,
				Asset:       v.Asset,
				Owner:       v.To,
				Type:        "transfer",
				TxId:        v.Id,
			})
		}
		if v.Type == "withdraw" {
			affectedAccounts[v.From] = true
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:          v.Id,
				BlockHeight: endBlock,
				Amount:      -v.Amount,
				Asset:       v.Asset,
				Owner:       v.From,
				Type:        "transfer",
				TxId:        v.Id,
			})
			ls.ActionsDb.StoreWithdrawal(ledgerDb.ActionRecord{
				Id:     v.Id,
				Amount: v.Amount,
				Asset:  v.Asset,
				To:     v.To,
				Memo:   v.Memo,
				TxId:   v.Id,
				Status: "pending",
				Type:   "withdraw",
			})
		}
		if v.Type == "stake" {
			affectedAccounts[v.From] = true
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:          v.Id,
				BlockHeight: endBlock,
				Amount:      -v.Amount,
				Asset:       "hbd",
				Owner:       v.From,
				Type:        "transfer",
				TxId:        v.Id,
			})
			ls.ActionsDb.StoreWithdrawal(
				ledgerDb.ActionRecord{
					Id:     v.Id,
					Amount: v.Amount,
					Asset:  "hbd_savings",
					To:     v.To,
					Memo:   v.Memo,
					TxId:   v.Id,
					Status: "pending",
					Type:   "stake",
				},
			)
		}
		if v.Type == "unstake" {
			affectedAccounts[v.From] = true
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:          v.Id,
				BlockHeight: endBlock,
				Amount:      -v.Amount,
				Asset:       "hbd_savings",
				Owner:       v.From,
				Type:        "transfer",
				TxId:        v.Id,
			})
			ls.ActionsDb.StoreWithdrawal(
				ledgerDb.ActionRecord{
					Id:     v.Id,
					Amount: v.Amount,
					Asset:  "hbd_savings",
					To:     v.To,
					Memo:   v.Memo,
					TxId:   v.Id,
					Status: "pending",
					Type:   "unstake",
				},
			)
		}
	}
	assets := []string{"hbd", "hive", "hbd_savings"}

	fmt.Println("Affected Accounts", affectedAccounts)
	//Cleanup!
	for k := range affectedAccounts {
		ledgerBalances := map[string]int64{}
		for _, asset := range assets {
			//As of block X or below
			bal := ls.GetBalance(k, endBlock, asset)
			fmt.Println("bal", bal)
			ledgerBalances[asset] = bal

			// ledgerUpdates, _ := ls.LedgerDb.GetLedgerRange(k, startHeight, endBlock, asset)

			// for _, v := range *ledgerUpdates {
			// 	ledgerBalances[asset] += v.Amount
			// }
		}
		ls.BalanceDb.UpdateBalanceRecord(k, endBlock, ledgerBalances)
	}
}

func (ls *LedgerSystem) SaveSnapshot(k string, endBlock uint64) {
	assets := []string{"hbd", "hive", "hbd_savings"}
	ledgerBalances := map[string]int64{}
	for _, asset := range assets {
		//As of block X or below
		bal := ls.GetBalance(k, endBlock, asset)
		fmt.Println("bal", bal)
		ledgerBalances[asset] = bal

		// ledgerUpdates, _ := ls.LedgerDb.GetLedgerRange(k, startHeight, endBlock, asset)

		// for _, v := range *ledgerUpdates {
		// 	ledgerBalances[asset] += v.Amount
		// }
	}
	ls.BalanceDb.UpdateBalanceRecord(k, endBlock, ledgerBalances)
}

type ExtraInfo struct {
	BlockHeight uint64
}

func (ls *LedgerSystem) ExecuteActions(actionsIds []string, extraInfo ExtraInfo) {
	for _, id := range actionsIds {
		record, _ := ls.ActionsDb.Get(id)
		if record == nil {
			continue
		}
		recordJson, _ := json.Marshal(record)
		fmt.Println("record", record)
		fmt.Println("recordJson", string(recordJson))

		if record.Type == "withdraw" {
			//literally nothing
		}
		if record.Type == "stake" {
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:     record.Id,
				Amount: record.Amount,
				Asset:  "hbd_savings",
				Owner:  record.To,
				Type:   "stake",

				//Next block balance should be clear
				BlockHeight: extraInfo.BlockHeight + 1,
				//Before everything
				BIdx:  -1,
				OpIdx: -1,
			})
		}
		if record.Type == "unstake" {
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:     record.Id,
				Amount: record.Amount,
				Asset:  "hbd",
				Owner:  record.To,
				Type:   "unstake",

				//It'll become available in 3 days of blocks
				BlockHeight: extraInfo.BlockHeight + HBD_UNSTAKE_BLOCKS,
				BIdx:        -1,
				OpIdx:       -1,
			})
		}
		ls.ActionsDb.SetStatus(id, "complete")
	}
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

	//Live calculated gateway balances on the fly
	//Use last saved balance as the starting data
	GatewayBalances map[string]uint64
}

// Empties the virtual state, such as when a block is executed
func (le *LedgerExecutor) Flush() {
	le.VirtualLedger = make(map[string][]LedgerUpdate)
	le.Oplog = make([]OpLogEvent, 0)
}

func (le *LedgerExecutor) Export() struct {
	Oplog []OpLogEvent
} {
	oplogCP := make([]OpLogEvent, len(le.Oplog))
	copy(oplogCP, le.Oplog)

	return struct {
		Oplog []OpLogEvent
	}{
		Oplog: oplogCP,
	}
}

type TransferOptions struct {
	//Excluded HBD amount that cannot be sent
	Exclusion int64
}

func (le *LedgerExecutor) ExecuteTransfer(OpLogEvent OpLogEvent, options ...TransferOptions) LedgerResult {
	//Check if the from account has enough balance

	exclusion := int64(0)

	if len(options) > 0 {
		options[0].Exclusion = exclusion
	}

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
	fromBal := le.SnapshotForAccount(OpLogEvent.From, OpLogEvent.BlockHeight, OpLogEvent.Asset)
	fmt.Println("fromBal", fromBal)
	if (fromBal - exclusion) < OpLogEvent.Amount {
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
	Id          string
	OpIdx       int64
	BIdx        int64
	BlockHeight uint64

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
		Id:          deposit.Id,
		BIdx:        deposit.BIdx,
		OpIdx:       deposit.OpIdx,
		BlockHeight: deposit.BlockHeight,

		Owner:  decodedParams.To,
		Amount: deposit.Amount,
		Asset:  deposit.Asset,
		Type:   "deposit",
	}
	le.VirtualLedger[decodedParams.To] = append(le.VirtualLedger[decodedParams.To], ledgerUpdate)
	le.Ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id:          deposit.Id,
		BlockHeight: deposit.BlockHeight,
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

	BIdx        int64  `json:"bidx"`
	OpIdx       int64  `json:"opidx"`
	BlockHeight uint64 `json:"block_height"`
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

	balAmt := le.SnapshotForAccount(withdraw.From, withdraw.BlockHeight, withdraw.Asset)

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

func (le *LedgerExecutor) CalculationFractStats(accountList []string, blockHeight uint64) struct {
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

	oplog := make([]OpLogEvent, len(le.Oplog))
	copy(oplog, le.Oplog)

	return &CompiledResult{
		OpLog: le.Oplog,
	}
}

type StakeOp struct {
	OpLogEvent
	Instant bool
	//If true, if stake/unstake fails, then regular op will occurr
	NoFail bool
}

const HBD_UNSTAKE_BLOCKS = uint64(86400)

// HBD instant stake/unstake fee
const HBD_INSTANT_FEE = int64(1) // 1% or 100 bps
const HBD_INSTANT_MIN = int64(1) // 0.001 HBD
const HBD_FEE_RECEIVER = "vsc.dao"

// Reserved for the future.
// Stake would trigger an indent to stake funds (immediately removing balance)
// Then trigger a delayed (actual stake) even when the onchain operation is executed through the gateway
// A two part Virtual Ledger operation operating out of sync
func (le *LedgerExecutor) Stake(stakeOp StakeOp, options ...TransferOptions) LedgerResult {

	exclusion := int64(0)

	if len(options) > 0 {
		options[0].Exclusion = exclusion
	}

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

	fromBal := le.SnapshotForAccount(stakeOp.From, stakeOp.BlockHeight, "hbd")

	if (exclusion + fromBal) < stakeOp.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "Insufficient balance",
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
			Memo:   "HBD_INSTANT_FEE",
		})
		le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], LedgerUpdate{
			Id:          stakeOp.Id,
			BlockHeight: stakeOp.BlockHeight,
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
		To:     stakeOp.To,
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

func (le *LedgerExecutor) Unstake(stakeOp StakeOp) LedgerResult {
	//Cannot unstake less than 0.002 HBD
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

	fromBal := le.SnapshotForAccount(stakeOp.From, stakeOp.BlockHeight, "hbd_savings")

	if fromBal < stakeOp.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "Insufficient balance",
		}
	}

	fee := int64(0)

	le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], LedgerUpdate{
		Id:     stakeOp.Id + "-in",
		OpIdx:  0,
		Owner:  stakeOp.From,
		Amount: -stakeOp.Amount,
		Asset:  "hbd_savingss",
		Type:   "unstake",
		Memo:   stakeOp.Memo,
	})

	if stakeOp.Instant {
		//withdrawAmount < currently available "safe" unstaked balance
		//enter in: some of kind of state tracking for "safe" unstaked balance

		fee = stakeOp.Amount * HBD_INSTANT_FEE / 100

		if fee == 0 {
			//Minimum of
			fee = HBD_INSTANT_MIN
		}
		withdrawAmount := stakeOp.Amount - fee

		le.VirtualLedger[HBD_FEE_RECEIVER] = append(le.VirtualLedger[HBD_FEE_RECEIVER], LedgerUpdate{
			Id:          stakeOp.Id + "-fee",
			BlockHeight: stakeOp.BlockHeight,
			OpIdx:       0,

			Owner:  HBD_FEE_RECEIVER,
			Amount: fee,
			Asset:  "hbd",
			Type:   "fee",
			Memo:   "HBD_INSTANT_FEE",
		})

		le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], LedgerUpdate{
			Id:          stakeOp.Id + "-out",
			BlockHeight: stakeOp.BlockHeight,
			OpIdx:       0,

			Owner:  stakeOp.To,
			Amount: withdrawAmount,
			Asset:  "hbd",
			Type:   "stake",
			Memo:   stakeOp.Memo,
		})
	}

	le.Oplog = append(le.Oplog, OpLogEvent{
		Id: stakeOp.Id + "-1",
		// Index: 1,

		From:   stakeOp.From,
		To:     stakeOp.To,
		Asset:  stakeOp.Asset,
		Amount: stakeOp.Amount,
		Memo:   stakeOp.Memo,
		Type:   "unstake",

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

func (le *LedgerExecutor) SnapshotForAccount(account string, blockHeight uint64, asset string) int64 {

	bal, _, err := le.Ls.BalanceDb.GetBalanceRecord(account, blockHeight, asset)
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
	FinalHeight uint64
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
