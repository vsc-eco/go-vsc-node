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
	"vsc-node/lib/logger"
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
	To     string `json:"to"`
	From   string `json:"from"`
	Amount int64  `json:"amount"`
	Asset  string `json:"asset"`
	Memo   string `json:"memo"`
	Type   string `json:"type"`

	//Not parted of compiled state
	Id          string `json:"id"`
	BIdx        int64  `json:"-"`
	OpIdx       int64  `json:"-"`
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

	log logger.Logger
}

// func (ls *LedgerSystem) ApplyOp(op LedgerOp) {

// }

func (ls *LedgerSystem) GetBalance(account string, blockHeight uint64, asset string) int64 {
	if !slices.Contains(assetTypes, asset) {
		return 0
	}

	balRecord, _ := ls.BalanceDb.GetBalanceRecord(account, blockHeight, asset)

	jsona, _ := json.Marshal(balRecord)
	ls.log.Debug("balRecord", string(jsona))
	if balRecord == nil {
		return 0
	}
	if asset == "hbd" {
		return balRecord.HBD
	} else if asset == "hive" {
		return balRecord.Hive
	} else if asset == "hbd_savings" {
		return balRecord.HBD_SAVINGS
	} else {
		return 0
	}

	// ledgerResults, _ := ls.LedgerDb.GetLedgerRange(account, lastHeight, blockHeight, asset)

	// fmt.Println("ledgerResults.len()", len(*ledgerResults))
	// for _, v := range *ledgerResults {
	// 	balRecord = balRecord + v.Amount
	// }
	// return balRecord
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

func (ls *LedgerSystem) ExecuteOplog(oplog []OpLogEvent, startHeight uint64, endBlock uint64) struct {
	accounts      []string
	ledgerRecords []LedgerUpdate
	actionRecords []ledgerDb.ActionRecord
} {
	affectedAccounts := map[string]bool{}

	ledgerRecords := make([]LedgerUpdate, 0)
	actionRecords := make([]ledgerDb.ActionRecord, 0)

	for _, v := range oplog {
		if v.Type == "transfer" {
			affectedAccounts[v.From] = true
			affectedAccounts[v.To] = true

			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id + "#in",
				Owner:       v.From,
				Amount:      -v.Amount,
				Asset:       v.Asset,
				Type:        "transfer",
				BlockHeight: endBlock,
			})
			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id + "#out",
				Owner:       v.From,
				Amount:      v.Amount,
				Asset:       v.Asset,
				Type:        "transfer",
				BlockHeight: endBlock,
			})

			// ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
			// 	Id:          v.Id,
			// 	BlockHeight: endBlock,
			// 	Amount:      -v.Amount,
			// 	Asset:       v.Asset,
			// 	Owner:       v.From,
			// 	Type:        "transfer",
			// 	TxId:        v.Id,
			// })
			// ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
			// 	Id:          v.Id,
			// 	BlockHeight: endBlock,
			// 	Amount:      v.Amount,
			// 	Asset:       v.Asset,
			// 	Owner:       v.To,
			// 	Type:        "transfer",
			// 	TxId:        v.Id,
			// })
		}
		if v.Type == "withdraw" {
			affectedAccounts[v.From] = true

			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id + "#in",
				Owner:       v.From,
				Amount:      -v.Amount,
				Asset:       v.Asset,
				Type:        "withdraw",
				BlockHeight: endBlock,
			})
			// ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
			// 	Id:          v.Id,
			// 	BlockHeight: endBlock,
			// 	Amount:      -v.Amount,
			// 	Asset:       v.Asset,
			// 	Owner:       v.From,
			// 	Type:        "withdraw",
			// 	TxId:        v.Id,
			// })

			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
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

			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id,
				Owner:       v.From,
				Amount:      -v.Amount,
				Asset:       "hbd",
				Type:        "stake",
				BlockHeight: endBlock,
			})
			// ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
			// 	Id:          v.Id,
			// 	BlockHeight: endBlock,
			// 	Amount:      -v.Amount,
			// 	Asset:       "hbd",
			// 	Owner:       v.From,
			// 	Type:        "stake",
			// 	TxId:        v.Id,
			// })
			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:          v.Id,
				Amount:      v.Amount,
				Asset:       "hbd_savings",
				To:          v.To,
				Memo:        v.Memo,
				TxId:        v.Id,
				Status:      "pending",
				Type:        "stake_hbd",
				BlockHeight: endBlock,
			})

		}
		if v.Type == "unstake" {
			affectedAccounts[v.From] = true

			ledgerRecords = append(ledgerRecords, LedgerUpdate{
				Id:          v.Id,
				BlockHeight: endBlock,
				Amount:      -v.Amount,
				Asset:       "hbd_savings",
				Owner:       v.From,
				Type:        "unstake",
			})
			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:          v.Id,
				Amount:      v.Amount,
				Asset:       "hbd_savings",
				To:          v.To,
				Memo:        v.Memo,
				TxId:        v.Id,
				Status:      "pending",
				Type:        "unstake_hbd",
				BlockHeight: endBlock,
			})
		}
	}
	// assets := []string{"hbd", "hive", "hbd_savings"}

	// fmt.Println("Affected Accounts", affectedAccounts)
	// //Cleanup!
	// for k := range affectedAccounts {
	// 	ledgerBalances := map[string]int64{}
	// 	for _, asset := range assets {
	// 		//As of block X or below
	// 		bal := ls.GetBalance(k, endBlock, asset)
	// 		fmt.Println("bal", bal)
	// 		ledgerBalances[asset] = bal

	// 		// ledgerUpdates, _ := ls.LedgerDb.GetLedgerRange(k, startHeight, endBlock, asset)

	// 		// for _, v := range *ledgerUpdates {
	// 		// 	ledgerBalances[asset] += v.Amount
	// 		// }
	// 	}
	// 	ls.BalanceDb.UpdateBalanceRecord(k, endBlock, ledgerBalances)
	// }

	accounts := make([]string, 0)
	for k := range affectedAccounts {
		accounts = append(accounts, k)
	}

	return struct {
		accounts      []string
		ledgerRecords []LedgerUpdate
		actionRecords []ledgerDb.ActionRecord
	}{
		accounts,
		ledgerRecords,
		actionRecords,
	}
}

func (ls *LedgerSystem) SaveSnapshot(k string, endBlock uint64) {
	assets := []string{"hbd", "hive", "hbd_savings"}
	ledgerBalances := map[string]int64{}
	for _, asset := range assets {
		//As of block X or below
		bal := ls.GetBalance(k, endBlock, asset)
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

func (ls *LedgerSystem) IndexActions(actionUpdate map[string]interface{}, extraInfo ExtraInfo) {
	ls.log.Debug("IndexActions", actionUpdate)

	actionIds := arrayToStringArray(actionUpdate["ops"].([]interface{}))

	for _, id := range actionIds {
		record, _ := ls.ActionsDb.Get(id)
		if record == nil {
			continue
		}

		ls.ActionsDb.ExecuteComplete(id)

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
	}

	//All stake related ops
	// completeOps := actionUpdate["stake_ops"].(string)

	// b64, _ := base64.RawURLEncoding.DecodeString(completeOps)

	// // bitset.
	// bs := big.Int{}

	// bs.SetBytes(b64)

	// stakedOps := make([]string, 0)
	// unstakeOps := make([]string, 0)

}

type OplogInjestOptions struct {
	StartHeight uint64
	EndHeight   uint64
}

func (ls *LedgerSystem) IngestOplog(oplog []OpLogEvent, options OplogInjestOptions) {
	affectedAccounts := map[string]bool{}
	affectedBalances := map[string]int64{}
	ledgerRecords := make([]ledgerDb.LedgerRecord, 0)
	actionRecords := make([]ledgerDb.ActionRecord, 0)

	oplogResults := ls.ExecuteOplog(oplog, options.StartHeight, options.EndHeight)

	ls.log.Debug("OplogResults", oplogResults)
	for _, v := range oplog {

		if v.Type == "transfer" {
			ledgerRecords = append(ledgerRecords, ledgerDb.LedgerRecord{
				Id:          v.Id + "#in",
				Amount:      -v.Amount,
				Asset:       v.Asset,
				From:        v.From,
				Owner:       v.From,
				Type:        "transfer",
				TxId:        v.Id,
				BlockHeight: v.BlockHeight,
			})

			ledgerRecords = append(ledgerRecords, ledgerDb.LedgerRecord{
				Id:          v.Id + "#out",
				Amount:      v.Amount,
				Asset:       v.Asset,
				Owner:       v.To,
				Type:        "transfer",
				TxId:        v.Id,
				BlockHeight: v.BlockHeight,
			})
			affectedBalances[v.From+v.Asset] = affectedBalances[v.From+v.Asset] - v.Amount
			affectedBalances[v.To+v.Asset] = affectedBalances[v.To+v.Asset] + v.Amount
			affectedAccounts[v.From] = true
			affectedAccounts[v.To] = true
		}
		if v.Type == "withdraw" {
			ledgerRecords = append(ledgerRecords, ledgerDb.LedgerRecord{
				Id:          v.Id + "#in",
				Amount:      -v.Amount,
				Asset:       v.Asset,
				From:        v.From,
				Owner:       v.From,
				Type:        "withdraw",
				TxId:        v.Id,
				BlockHeight: v.BlockHeight,
			})
			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:     v.Id,
				Amount: v.Amount,
				Asset:  v.Asset,
				To:     v.To,
				Memo:   v.Memo,
				TxId:   v.Id,
				Status: "pending",
				Type:   "withdraw",
			})
			affectedBalances[v.From+v.Asset] = affectedBalances[v.From+v.Asset] - v.Amount
			affectedAccounts[v.From] = true
		}
		if v.Type == "stake" {
			affectedAccounts[v.From] = true

			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id: v.Id + "#in",
				//plz passthrough original block height
				BlockHeight: options.EndHeight,
				Amount:      -v.Amount,
				Asset:       "hbd",
				Owner:       v.From,
				Type:        "stake",
				TxId:        v.Id,
			})
			ls.ActionsDb.StoreAction(
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
				Id: v.Id + "#in",
				////plz passthrough original block height
				BlockHeight: options.EndHeight,
				Amount:      -v.Amount,
				Asset:       "hbd_savings",
				Owner:       v.From,
				Type:        "transfer",
				TxId:        v.Id,
			})
			ls.ActionsDb.StoreAction(
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
	for _, v := range ledgerRecords {
		ls.LedgerDb.StoreLedger(v)
	}

	for _, v := range actionRecords {
		ls.ActionsDb.StoreAction(v)
	}

	distinctAccounts, _ := ls.LedgerDb.GetDistinctAccountsRange(options.StartHeight, options.EndHeight)

	assets := []string{"hbd", "hive", "hbd_savings", "consensus"}

	//Cleanup!
	for _, k := range distinctAccounts {
		ledgerBalances := map[string]int64{}
		for _, asset := range assets {
			//As of block X or below
			bal := ls.GetBalance(k, options.StartHeight, asset)
			ledgerBalances[asset] = bal
			fmt.Println("LedgerBalance", bal, asset)

			ledgerUpdates, _ := ls.LedgerDb.GetLedgerRange(k, options.StartHeight, options.EndHeight, asset)

			for _, v := range *ledgerUpdates {
				ledgerBalances[asset] += v.Amount
			}
		}
		ls.BalanceDb.UpdateBalanceRecord(k, options.EndHeight, ledgerBalances)
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

type LedgerSession struct {
	le *LedgerExecutor

	oplog     []OpLogEvent
	ledgerOps []LedgerUpdate
	balances  map[string]*int64

	StartHeight uint64
}

func (lss *LedgerSession) Done() {
	lss.le.Oplog = append(lss.le.Oplog, lss.oplog...)
	for _, op := range lss.ledgerOps {
		lss.le.Ls.log.Debug("LedgerSession.Done adding ledgerResult", op)
		lss.le.VirtualLedger[op.Owner] = append(lss.le.VirtualLedger[op.Owner], op)
	}
	lss.balances = make(map[string]*int64)
	lss.oplog = make([]OpLogEvent, 0)
	lss.ledgerOps = make([]LedgerUpdate, 0)
}

func (lss *LedgerSession) Revert() {
	lss.oplog = make([]OpLogEvent, 0)
	lss.ledgerOps = make([]LedgerUpdate, 0)
	lss.balances = make(map[string]*int64)
}

// Appends an Oplog with no validation
func (lss *LedgerSession) AppendOplog(event OpLogEvent) {
	result := lss.le.Ls.ExecuteOplog([]OpLogEvent{event}, lss.StartHeight, event.BlockHeight)

	for _, v := range result.ledgerRecords {
		lss.AppendLedger(v)
	}

	lss.oplog = append(lss.oplog, event)
}

func (lss *LedgerSession) Transfer() {
	//pass to LE
}

// Appends an ledger with no validation
func (lss *LedgerSession) AppendLedger(event LedgerUpdate) {
	bal := lss.GetBalance(event.Owner, event.BlockHeight, event.Asset)
	lss.setBalance(event.Owner, event.Asset, bal+event.Amount)

	lss.ledgerOps = append(lss.ledgerOps, event)
}

func (lss *LedgerSession) GetBalance(account string, blockHeight uint64, asset string) int64 {
	if lss.balances[lss.key(account, asset)] == nil {
		bal := lss.le.SnapshotForAccount(account, blockHeight, asset)
		lss.balances[lss.key(account, asset)] = &bal
	}

	return *lss.balances[lss.key(account, asset)]
}

func (lss *LedgerSession) setBalance(account string, asset string, amount int64) {
	lss.balances[lss.key(account, asset)] = &amount
}

func (lss *LedgerSession) key(account, asset string) string {
	return account + "#" + asset
}

func (le *LedgerExecutor) NewSession(startHeight uint64) *LedgerSession {
	return &LedgerSession{
		balances:    make(map[string]*int64),
		oplog:       make([]OpLogEvent, 0),
		ledgerOps:   make([]LedgerUpdate, 0),
		StartHeight: startHeight,
		le:          le,
	}
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

func (ledgerSession *LedgerSession) ExecuteTransfer(OpLogEvent OpLogEvent, options ...TransferOptions) LedgerResult {
	le := ledgerSession.le
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
	fromBal := ledgerSession.GetBalance(OpLogEvent.From, OpLogEvent.BlockHeight, OpLogEvent.Asset)

	le.Ls.log.Debug("Transfer - balAmt", fromBal, "bh="+strconv.Itoa(int(OpLogEvent.BlockHeight)))

	if (fromBal - exclusion) < OpLogEvent.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "Insufficient balance",
		}
	}

	OpLogEvent.Type = "transfer"

	ledgerSession.AppendOplog(OpLogEvent)

	// le.Oplog = append(le.Oplog, OpLogEvent)

	// le.VirtualLedger[OpLogEvent.From] = append(le.VirtualLedger[OpLogEvent.From], LedgerUpdate{
	// 	Id:    OpLogEvent.Id + "#0",
	// 	OpIdx: OpLogEvent.OpIdx,

	// 	Owner:  OpLogEvent.From,
	// 	Amount: -OpLogEvent.Amount,
	// 	Asset:  OpLogEvent.Asset,
	// 	Type:   "transfer",
	// })

	// le.VirtualLedger[OpLogEvent.To] = append(le.VirtualLedger[OpLogEvent.To], LedgerUpdate{
	// 	Id:    OpLogEvent.Id + "#1",
	// 	OpIdx: OpLogEvent.OpIdx,
	// 	BIdx:  OpLogEvent.BIdx,

	// 	Owner:  OpLogEvent.To,
	// 	Amount: OpLogEvent.Amount,
	// 	Asset:  OpLogEvent.Asset,
	// 	Type:   "transfer",
	// })
	// 	BIdx:  OpLogEvent.BIdx,

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
		// fmt.Println("decodedParams.To", decodedParams.To)
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
	} else if strings.HasPrefix(decodedParams.To, "did:") {
		//No nothing. It's parsed correctly
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
	// le.AppendLedger(ledgerUpdate)

	le.Ls.log.Debug("ledgerExecutor", le.VirtualLedger[decodedParams.To])
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

func (le *LedgerExecutor) AppendLedger(update LedgerUpdate) {
	key := update.Owner + "#" + update.Asset
	if le.GatewayBalances[key] == 0 {
		le.Ls.GetBalance(update.Owner, update.BlockHeight, update.Asset)
	}
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

func (ledgerSession *LedgerSession) Withdraw(withdraw WithdrawParams) LedgerResult {
	le := ledgerSession.le
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

	balAmt := ledgerSession.GetBalance(withdraw.From, withdraw.BlockHeight, withdraw.Asset)

	le.Ls.log.Debug("Withdraw - balAmt", balAmt, withdraw.Id)

	if balAmt < withdraw.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "Insufficient balance",
		}
	}

	ledgerSession.AppendOplog(OpLogEvent{
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

func (le *LedgerExecutor) Compile(bh uint64) *CompiledResult {
	if len(le.Oplog) == 0 {
		return nil
	}
	oplog := make([]OpLogEvent, 0)
	// copy(oplog, le.Oplog)

	for _, v := range le.Oplog {
		//bh should be == slot height
		if v.BlockHeight <= bh {
			oplog = append(oplog, v)
		}
	}

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
func (le *LedgerExecutor) Stake(stakeOp StakeOp, ledgerSession *LedgerSession, options ...TransferOptions) LedgerResult {

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

	fromBal := ledgerSession.GetBalance(stakeOp.From, stakeOp.BlockHeight, "hbd")
	// fromBal := le.SnapshotForAccount(stakeOp.From, stakeOp.BlockHeight, "hbd")

	le.Ls.log.Debug("Stake - balAmt", fromBal, stakeOp.Id)

	if (exclusion + fromBal) < stakeOp.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "Insufficient balance",
		}
	}

	ledgerSession.AppendLedger(LedgerUpdate{
		Id:     stakeOp.Id,
		OpIdx:  0,
		Owner:  stakeOp.From,
		Amount: -stakeOp.Amount,
		Asset:  stakeOp.Asset,
		Type:   "stake",
		Memo:   stakeOp.Memo,
	})

	// le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], LedgerUpdate{
	// 	Id:     stakeOp.Id,
	// 	OpIdx:  0,
	// 	Owner:  stakeOp.From,
	// 	Amount: -stakeOp.Amount,
	// 	Asset:  stakeOp.Asset,
	// 	Type:   "stake",
	// 	Memo:   stakeOp.Memo,
	// })

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

		ledgerSession.AppendLedger(LedgerUpdate{
			Id:     stakeOp.Id + "#fee",
			OpIdx:  0,
			Owner:  HBD_FEE_RECEIVER,
			Amount: fee,
			Asset:  "hbd",
			Type:   "fee",
			Memo:   "HBD_INSTANT_FEE",
		})
		le.VirtualLedger[HBD_FEE_RECEIVER] = append(le.VirtualLedger[HBD_FEE_RECEIVER], LedgerUpdate{
			Id:     stakeOp.Id + "#fee",
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
		Id: stakeOp.Id,
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

func (le *LedgerExecutor) Unstake(stakeOp StakeOp, ledgerSession *LedgerSession) LedgerResult {
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
			Id:          stakeOp.Id + "#fee",
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
		Id: stakeOp.Id,
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
	bal := le.Ls.GetBalance(account, blockHeight, asset)

	le.Ls.log.Debug("getBalance le.VirtualLedger["+account+"]", le.VirtualLedger[account], blockHeight)
	for _, v := range le.VirtualLedger[account] {
		//Must be ledger ops with height below or equal to the current block height
		//Current block height ledger ops are recently executed
		if v.Asset == asset {
			bal += v.Amount
		}
	}
	return bal
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
