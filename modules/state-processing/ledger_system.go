package stateEngine

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"vsc-node/lib/logger"
	"vsc-node/modules/common"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
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

var transferableAssetTypes = []string{"hive", "hbd", "hbd_savings"}
var assetTypes = slices.Concat(transferableAssetTypes, []string{"hive_consensus"})

const ETH_REGEX = "^0x[a-fA-F0-9]{40}$"
const HIVE_REGEX = `^[a-z][0-9a-z\-]*[0-9a-z](\.[a-z][0-9a-z\-]*[0-9a-z])*$`

type LedgerSystem struct {
	BalanceDb ledgerDb.Balances
	LedgerDb  ledgerDb.Ledger
	ClaimDb   ledgerDb.InterestClaims

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

	balRecordPtr, _ := ls.BalanceDb.GetBalanceRecord(account, blockHeight)

	var recordHeight uint64
	var balRecord ledgerDb.BalanceRecord
	if balRecordPtr == nil {
		recordHeight = 0
	} else {
		balRecord = *balRecordPtr
		recordHeight = balRecord.BlockHeight + 1
	}
	if asset == "hbd" {
		ledgerResults, _ := ls.LedgerDb.GetLedgerRange(account, recordHeight, blockHeight, asset, ledgerDb.LedgerOptions{
			OpType: []string{"unstake", "deposit"},
		})

		balAdjust := int64(0)

		for _, v := range *ledgerResults {
			balAdjust += v.Amount
		}

		return balRecord.HBD + balAdjust
	} else if asset == "hive" {
		ledgerResults, _ := ls.LedgerDb.GetLedgerRange(account, recordHeight, blockHeight, asset, ledgerDb.LedgerOptions{
			OpType: []string{"deposit"},
		})

		balAdjust := int64(0)

		for _, v := range *ledgerResults {
			balAdjust += v.Amount
		}

		return balRecord.Hive + balAdjust
	} else if asset == "hbd_savings" {

		ledgerResults, _ := ls.LedgerDb.GetLedgerRange(account, recordHeight, blockHeight, asset, ledgerDb.LedgerOptions{
			OpType: []string{"stake"},
		})

		stakeBal := int64(0)

		for _, v := range *ledgerResults {
			stakeBal += v.Amount
		}
		ls.log.Debug("GetBalance HBD_SAVINGS-BAL", stakeBal, balRecord, blockHeight)

		return balRecord.HBD_SAVINGS + stakeBal
	} else if asset == "hive_consensus" {
		return balRecord.HIVE_CONSENSUS
	} else {
		return 0
	}

	// ledgerSystem.LedgerResults, _ := ls.LedgerDb.GetLedgerRange(account, lastHeight, blockHeight, asset)

	// fmt.Println("ledgerSystem.LedgerResults.len()", len(*ledgerSystem.LedgerResults))
	// for _, v := range *ledgerSystem.LedgerResults {
	// 	balRecord = balRecord + v.Amount
	// }
	// return balRecord
}

func (ls *LedgerSystem) ClaimHBDInterest(lastClaim uint64, blockHeight uint64, amount int64) {
	fmt.Println("ClaimHBDInterest", lastClaim, blockHeight, amount)
	//Do distribution of HBD interest on an going forward basis
	//Save to ledger DB the difference.
	ledgerBalances := ls.BalanceDb.GetAll(blockHeight)

	processedBalRecords := make([]ledgerDb.BalanceRecord, 0)
	totalAvg := int64(0)
	//Ensure averages have been updated before distribution;
	for _, balance := range ledgerBalances {

		A := blockHeight - balance.HBD_MODIFY_HEIGHT //Example: Modifed at 10, endBlock = 40; thus 10-40 = 30
		B := blockHeight - balance.HBD_CLAIM_HEIGHT  //Example: Claimed at block 100, current block 800; thus

		moreAvg := balance.HBD_SAVINGS * int64(A) / int64(B)

		var tmpAvg int64
		if moreAvg > 0 {
			tmpAvg = balance.HBD_AVG + moreAvg
		} else {
			tmpAvg = balance.HBD_AVG
		}

		//Apply adjustments
		endingAvg := tmpAvg * int64(A) / int64(B)

		if endingAvg < 1 {
			fmt.Println("ClaimHBD tmpAvg is sub zero", balance.Account, tmpAvg)
			continue
		}

		balance.HBD_AVG = endingAvg

		processedBalRecords = append(processedBalRecords, balance)
		totalAvg = totalAvg + endingAvg
	}

	bsj, _ := json.Marshal(processedBalRecords)

	fmt.Println("Processed bal records", ledgerBalances, string(bsj))

	for id, balance := range processedBalRecords {
		// if balance.HBD_AVG == 0 {
		// 	continue
		// }

		distributeAmt := balance.HBD_AVG * amount / totalAvg

		if distributeAmt > 0 {
			var owner string
			if strings.HasPrefix(balance.Account, "system:") {
				if balance.Account == "system:fr_balance" {
					owner = common.DAO_WALLET
				} else {
					//Filter
					continue
				}
			} else {
				owner = balance.Account
			}

			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id: "hbd_interest_" + strconv.Itoa(int(blockHeight)) + "_" + strconv.Itoa(id),
				//next block
				BlockHeight: blockHeight + 1,
				Amount:      int64(distributeAmt),
				Asset:       "hbd_savings",
				Owner:       owner,
				Type:        "interest",
			})
		}
	}
	//Note this calculation is inaccurate and should be calculated based on N blocks of Y claim period
	//Should not assume a static amount of time or 12 exactly claim interals per year. But since this is for statistics, it doesn't matter.
	observedApr := (float64(amount) / float64(totalAvg)) * 12

	ls.ClaimDb.SaveClaim(ledgerDb.ClaimRecord{
		BlockHeight: blockHeight,
		Amount:      amount,
		ReceivedN:   len(processedBalRecords),
		ObservedApr: observedApr,
	})
	//DONE
}

func (ls *LedgerSystem) ExecuteOplog(oplog []ledgerSystem.OpLogEvent, startHeight uint64, endBlock uint64) struct {
	accounts      []string
	ledgerRecords []ledgerSystem.LedgerUpdate
	actionRecords []ledgerDb.ActionRecord
} {
	affectedAccounts := map[string]bool{}

	ledgerRecords := make([]ledgerSystem.LedgerUpdate, 0)
	actionRecords := make([]ledgerDb.ActionRecord, 0)

	for _, v := range oplog {
		if v.Type == "transfer" {
			affectedAccounts[v.From] = true
			affectedAccounts[v.To] = true

			ledgerRecords = append(ledgerRecords, ledgerSystem.LedgerUpdate{
				Id:          v.Id + "#in",
				Owner:       v.From,
				Amount:      -v.Amount,
				Asset:       v.Asset,
				Type:        "transfer",
				BlockHeight: endBlock,
			})
			ledgerRecords = append(ledgerRecords, ledgerSystem.LedgerUpdate{
				Id:          v.Id + "#out",
				Owner:       v.To,
				Amount:      v.Amount,
				Asset:       v.Asset,
				Type:        "transfer",
				BlockHeight: endBlock,
			})
		}
		if v.Type == "withdraw" {
			affectedAccounts[v.From] = true

			ledgerRecords = append(ledgerRecords, ledgerSystem.LedgerUpdate{
				Id:          v.Id + "#in",
				Owner:       v.From,
				Amount:      -v.Amount,
				Asset:       v.Asset,
				Type:        "withdraw",
				BlockHeight: endBlock,
			})

			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:          v.Id,
				Amount:      v.Amount,
				Asset:       v.Asset,
				To:          v.To,
				Memo:        v.Memo,
				TxId:        v.Id,
				Status:      "pending",
				Type:        "withdraw",
				BlockHeight: v.BlockHeight,
			})
		}
		if v.Type == "stake" {
			affectedAccounts[v.From] = true

			ledgerRecords = append(ledgerRecords, ledgerSystem.LedgerUpdate{
				Id:          v.Id + "#in",
				Owner:       v.From,
				Amount:      -v.Amount,
				Asset:       "hbd",
				Type:        "stake",
				BlockHeight: endBlock,
			})

			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:          v.Id,
				Amount:      v.Amount,
				Asset:       "hbd_savings",
				To:          v.To,
				Memo:        v.Memo,
				TxId:        v.Id,
				Status:      "pending",
				Type:        "stake",
				BlockHeight: endBlock,
			})

		}
		if v.Type == "unstake" {
			affectedAccounts[v.From] = true

			ledgerRecords = append(ledgerRecords, ledgerSystem.LedgerUpdate{
				Id:          v.Id + "#in",
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
				Type:        "unstake",
				BlockHeight: endBlock,
			})
		}
		if v.Type == "consensus_stake" {
			ledgerRecords = append(ledgerRecords, ledgerSystem.LedgerUpdate{
				Id:          v.Id + "#in",
				BlockHeight: endBlock,
				Amount:      -v.Amount,
				Asset:       "hive",
				Owner:       v.From,
				Type:        "consensus_stake",
			})
			ledgerRecords = append(ledgerRecords, ledgerSystem.LedgerUpdate{
				Id:          v.Id + "#out",
				BlockHeight: endBlock,
				Amount:      v.Amount,
				Asset:       "hive_consensus",
				Owner:       v.To,
				Type:        "consensus_stake",
			})
		}
		if v.Type == "consensus_unstake" {
			ledgerRecords = append(ledgerRecords, ledgerSystem.LedgerUpdate{
				Id:          v.Id + "#in",
				BlockHeight: endBlock,
				Amount:      -v.Amount,
				Asset:       "hive_consensus",
				Owner:       v.From,
				Type:        "consensus_unstake",
			})

			actionRecords = append(actionRecords, ledgerDb.ActionRecord{
				Id:     v.Id,
				Amount: v.Amount,
				Asset:  "-",
				To:     v.To,
				Memo:   v.Memo,
				TxId:   v.Id,
				Status: "pending",
				Type:   "consensus_unstake",
				Params: map[string]interface{}{
					"epoch": v.Params["epoch"],
				},
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

	// 		// ledgerSystem.ledgerSystem.LedgerUpdates, _ := ls.LedgerDb.GetLedgerRange(k, startHeight, endBlock, asset)

	// 		// for _, v := range *ledgerSystem.ledgerSystem.LedgerUpdates {
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
		ledgerRecords []ledgerSystem.LedgerUpdate
		actionRecords []ledgerDb.ActionRecord
	}{
		accounts,
		ledgerRecords,
		actionRecords,
	}
}

// func (ls *LedgerSystem) SaveSnapshot(k string, endBlock uint64) {
// 	assets := []string{"hbd", "hive", "hbd_savings"}
// 	ledgerBalances := map[string]int64{}
// 	for _, asset := range assets {
// 		//As of block X or below
// 		bal := ls.GetBalance(k, endBlock, asset)
// 		ledgerBalances[asset] = bal

// 		// ledgerSystem.ledgerSystem.LedgerUpdates, _ := ls.LedgerDb.GetLedgerRange(k, startHeight, endBlock, asset)

// 		// for _, v := range *ledgerSystem.LedgerUpdates {
// 		// 	ledgerBalances[asset] += v.Amount
// 		// }
// 	}
// 	ls.BalanceDb.UpdateBalanceRecord(ledgerDb.BalanceRecord{
// 		Account:     k,
// 		BlockHeight: endBlock,
// 		Hive:        ledgerBalances["hive"],
// 		HBD:         ledgerBalances["hbd"],
// 		HBD_SAVINGS: ledgerBalances["hbd_savings"],

// 	})
// 	// ls.BalanceDb.UpdateBalanceRecord(k, endBlock, ledgerBalances)
// }

type ExtraInfo struct {
	BlockHeight uint64
	ActionId    string
}

func (ls *LedgerSystem) IndexActions(actionUpdate map[string]interface{}, extraInfo ExtraInfo) {
	ls.log.Debug("IndexActions", actionUpdate)

	actionIds := common.ArrayToStringArray(actionUpdate["ops"].([]interface{}))

	//All stake related ops
	completeOps := actionUpdate["cleared_ops"].(string)

	b64, _ := base64.RawURLEncoding.DecodeString(completeOps)

	bs := big.Int{}

	bs.SetBytes(b64)

	for idx, id := range actionIds {
		record, _ := ls.ActionsDb.Get(id)
		if record == nil {
			continue
		}
		ls.ActionsDb.ExecuteComplete(&extraInfo.ActionId, id)

		if record.Type == "stake" {
			ls.log.Debug("Indexxing stake Ledger")
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:     record.Id + "#out",
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
			var blockDelay uint64

			//If is a neutral stake op, then set only 1 block delay
			if bs.Bit(idx) == 1 {
				blockDelay = 1
			} else {
				blockDelay = HBD_UNSTAKE_BLOCKS
			}
			ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
				Id:     record.Id + "#out",
				Amount: record.Amount,
				Asset:  "hbd",
				Owner:  record.To,
				Type:   "unstake",

				//It'll become available in 3 days of blocks
				BlockHeight: extraInfo.BlockHeight + blockDelay,
				BIdx:        -1,
				OpIdx:       -1,
			})
		}
	}

}

type OplogInjestOptions struct {
	StartHeight uint64
	EndHeight   uint64
}

func (ls *LedgerSystem) IngestOplog(oplog []ledgerSystem.OpLogEvent, options OplogInjestOptions) {
	executeResults := ls.ExecuteOplog(oplog, options.StartHeight, options.EndHeight)

	ledgerRecords := executeResults.ledgerRecords
	actionRecords := executeResults.actionRecords

	for _, v := range ledgerRecords {
		ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
			Id: v.Id,
			//plz passthrough original block height
			BlockHeight: options.EndHeight,
			Amount:      v.Amount,
			Asset:       v.Asset,
			Owner:       v.Owner,
			Type:        v.Type,
			// TxId:        v.,
		})
	}

	for _, v := range actionRecords {
		ls.ActionsDb.StoreAction(v)
	}

}

// Used during live execution of transfers such as those from contracts or user transaction
type LedgerExecutor struct {
	Ls *LedgerSystem

	//List of finalized operations such as transfers, withdrawals.
	//Expected to be identical when block is produced and used during block creation
	Oplog []ledgerSystem.OpLogEvent
	//Virtual ledger is a cache of all balance changes (virtual and non-virtual)
	//Includes deposits, transfers (in-n-out), withdrawals, and stake/unstake operations (future)
	VirtualLedger map[string][]ledgerSystem.LedgerUpdate

	//Live calculated gateway balances on the fly
	//Use last saved balance as the starting data
	GatewayBalances map[string]uint64
}

type LedgerSession struct {
	le *LedgerExecutor

	oplog     []ledgerSystem.OpLogEvent
	ledgerOps []ledgerSystem.LedgerUpdate
	balances  map[string]*int64
	idCache   map[string]int

	StartHeight uint64
}

func (lss *LedgerSession) Done() []string {
	// oplog := make([]ledgerSystem.OpLogEvent, len(lss.oplog))
	// copy(oplog, lss.oplog)
	ledgerIds := make([]string, 0)
	for _, v := range lss.oplog {
		ledgerIds = append(ledgerIds, v.Id)
	}

	lss.le.Oplog = append(lss.le.Oplog, lss.oplog...)
	for _, op := range lss.ledgerOps {
		lss.le.Ls.log.Debug("LedgerSession.Done adding ledgerSystem.LedgerResult", op)
		lss.le.VirtualLedger[op.Owner] = append(lss.le.VirtualLedger[op.Owner], op)
	}
	lss.balances = make(map[string]*int64)
	lss.oplog = make([]ledgerSystem.OpLogEvent, 0)
	lss.ledgerOps = make([]ledgerSystem.LedgerUpdate, 0)

	return ledgerIds
}

func (lss *LedgerSession) Revert() {
	lss.oplog = make([]ledgerSystem.OpLogEvent, 0)
	lss.ledgerOps = make([]ledgerSystem.LedgerUpdate, 0)
	lss.balances = make(map[string]*int64)
}

// Appends an Oplog with no validation
func (lss *LedgerSession) AppendOplog(event ledgerSystem.OpLogEvent) {
	//Maybe this should be calculated upon indexing rather than before
	id2 := event.Id
	if lss.idCache[id2] > 0 {
		event.Id = id2 + ":" + strconv.Itoa(lss.idCache[id2])
	}
	// lss.le.Ls.log.Debug("AppendOplog event ID", event, lss.idCache[id2])
	lss.idCache[id2]++

	result := lss.le.Ls.ExecuteOplog([]ledgerSystem.OpLogEvent{event}, lss.StartHeight, event.BlockHeight)

	for _, v := range result.ledgerRecords {
		lss.AppendLedger(v)
	}

	lss.oplog = append(lss.oplog, event)
}

func (lss *LedgerSession) Transfer() {
	//pass to LE
}

// Appends an ledger with no validation
func (lss *LedgerSession) AppendLedger(event ledgerSystem.LedgerUpdate) {
	// lss.le.Ls.log.Debug("LedgerSession.AppendLedger GetBalance")
	bal := lss.GetBalance(event.Owner, event.BlockHeight, event.Asset)
	lss.setBalance(event.Owner, event.Asset, bal+event.Amount)

	lss.ledgerOps = append(lss.ledgerOps, event)
}

func (lss *LedgerSession) GetBalance(account string, blockHeight uint64, asset string) int64 {
	if lss.balances[lss.key(account, asset)] == nil {
		bal := lss.le.SnapshotForAccount(account, blockHeight, asset)
		lss.balances[lss.key(account, asset)] = &bal
		fmt.Println("Ledger get Current lol")
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
		oplog:       make([]ledgerSystem.OpLogEvent, 0),
		ledgerOps:   make([]ledgerSystem.LedgerUpdate, 0),
		idCache:     make(map[string]int),
		StartHeight: startHeight,
		le:          le,
	}
}

// Empties the virtual state, such as when a block is executed
func (le *LedgerExecutor) Flush() {
	le.VirtualLedger = make(map[string][]ledgerSystem.LedgerUpdate)
	le.Oplog = make([]ledgerSystem.OpLogEvent, 0)
}

func (le *LedgerExecutor) Export() struct {
	Oplog []ledgerSystem.OpLogEvent
} {
	oplogCP := make([]ledgerSystem.OpLogEvent, len(le.Oplog))
	copy(oplogCP, le.Oplog)

	return struct {
		Oplog []ledgerSystem.OpLogEvent
	}{
		Oplog: oplogCP,
	}
}

func (ledgerSession *LedgerSession) ExecuteTransfer(opLogEvent ledgerSystem.OpLogEvent, options ...ledgerSystem.TransferOptions) ledgerSystem.LedgerResult {
	le := ledgerSession.le
	//Check if the from account has enough balance
	exclusion := int64(0)

	if len(options) > 0 {
		options[0].Exclusion = exclusion
	}

	if opLogEvent.Amount <= 0 {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}
	if opLogEvent.To == opLogEvent.From {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "cannot send to self",
		}
	}
	if strings.HasPrefix(opLogEvent.To, "system:") {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid destination",
		}
	}
	if !slices.Contains(transferableAssetTypes, opLogEvent.Asset) {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid asset",
		}
	}
	fromBal := ledgerSession.GetBalance(opLogEvent.From, opLogEvent.BlockHeight, opLogEvent.Asset)

	le.Ls.log.Debug("Transfer - balAmt", fromBal, "bh="+strconv.Itoa(int(opLogEvent.BlockHeight)))

	results, _ := le.Ls.LedgerDb.GetLedgerRange(opLogEvent.From, ledgerSession.StartHeight, opLogEvent.BlockHeight, opLogEvent.Asset)

	le.Ls.log.Debug("Ledger results", *results)

	le.Ls.log.Debug("ledgerSession.StartHeight", ledgerSession.StartHeight, "ledgerSystem.OpLogEvent.BlockHeight", opLogEvent.BlockHeight)

	fmt.Println("Ledger.Status", opLogEvent.From, fromBal, opLogEvent.Amount)
	if (fromBal - exclusion) < opLogEvent.Amount {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}
	opLogEvent.Type = "transfer"

	ledgerSession.AppendOplog(opLogEvent)

	return ledgerSystem.LedgerResult{
		Ok:  true,
		Msg: "success",
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

func (le *LedgerExecutor) Deposit(deposit Deposit) string {
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
		le.VirtualLedger = make(map[string][]ledgerSystem.LedgerUpdate)
	}

	if le.VirtualLedger[decodedParams.To] != nil {
		le.Ls.log.Debug("ledgerExecutor", le.VirtualLedger[decodedParams.To])
	}

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

	return decodedParams.To
}

func (le *LedgerExecutor) AppendLedger(update ledgerSystem.LedgerUpdate) {
	key := update.Owner + "#" + update.Asset
	if le.GatewayBalances[key] == 0 {
		le.Ls.GetBalance(update.Owner, update.BlockHeight, update.Asset)
	}
}

func (ledgerSession *LedgerSession) Withdraw(withdraw ledgerSystem.WithdrawParams) ledgerSystem.LedgerResult {
	le := ledgerSession.le
	if withdraw.Amount <= 0 {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	if !slices.Contains([]string{"hive", "hbd"}, withdraw.Asset) {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid asset",
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
			return ledgerSystem.LedgerResult{
				Ok:  false,
				Msg: "invalid destination",
			}
		}
	} else {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid destination",
		}
	}

	balAmt := ledgerSession.GetBalance(withdraw.From, withdraw.BlockHeight, withdraw.Asset)

	le.Ls.log.Debug("Withdraw - balAmt", balAmt, withdraw.Id)

	if balAmt < withdraw.Amount {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	ledgerSession.AppendOplog(ledgerSystem.OpLogEvent{
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

	return ledgerSystem.LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

func (le *LedgerExecutor) ConsensusStake(params ledgerSystem.ConsensusParams, ledgerSession *LedgerSession) ledgerSystem.LedgerResult {

	if params.Amount <= 0 {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	// if !slices.Contains(assetTypes, withdraw.Asset) {
	// 	return ledgerSystem.LedgerResult{
	// 		Ok:  false,
	// 		Msg: "Invalid asset",
	// 	}
	// }

	balAmt := ledgerSession.GetBalance(params.From, params.BlockHeight, "hive")

	if balAmt < params.Amount {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	ledgerSession.AppendOplog(ledgerSystem.OpLogEvent{
		Id:          params.Id,
		From:        params.From,
		To:          params.To,
		BlockHeight: params.BlockHeight,

		Amount: params.Amount,
		Asset:  "hive",
		Type:   "consensus_stake",
	})

	return ledgerSystem.LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

func (le *LedgerExecutor) ConsensusUnstake(params ledgerSystem.ConsensusParams, ledgerSession *LedgerSession) ledgerSystem.LedgerResult {
	if params.Amount <= 0 {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	balAmt := ledgerSession.GetBalance(params.From, params.BlockHeight, "hive_consensus")

	if balAmt < params.Amount {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	ledgerSession.AppendOplog(ledgerSystem.OpLogEvent{
		Id:          params.Id,
		To:          params.To,
		From:        params.From,
		BlockHeight: params.BlockHeight,

		Amount: params.Amount,
		Asset:  "hive",
		Type:   "consensus_unstake",

		Params: map[string]interface{}{
			"epoch": params.ElectionEpoch,
		},
	})

	return ledgerSystem.LedgerResult{
		Ok:  true,
		Msg: "success",
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
	OpLog []ledgerSystem.OpLogEvent
}

func (le *LedgerExecutor) Compile(bh uint64) *CompiledResult {
	if len(le.Oplog) == 0 {
		return nil
	}
	oplog := make([]ledgerSystem.OpLogEvent, 0)
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
	ledgerSystem.OpLogEvent
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
func (le *LedgerExecutor) Stake(stakeOp StakeOp, ledgerSession *LedgerSession, options ...ledgerSystem.TransferOptions) ledgerSystem.LedgerResult {

	exclusion := int64(0)

	if len(options) > 0 {
		options[0].Exclusion = exclusion
	}

	//Cannot stake less than 0.002 HBD
	//As there is a 0.001 HBD fee for instant staking
	if stakeOp.Amount <= 0 || (stakeOp.Instant && stakeOp.Amount < 2) {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	if stakeOp.Asset != "hbd" {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid asset",
		}
	}

	fromBal := ledgerSession.GetBalance(stakeOp.From, stakeOp.BlockHeight, "hbd")
	// fromBal := le.SnapshotForAccount(stakeOp.From, stakeOp.BlockHeight, "hbd")

	le.Ls.log.Debug("Stake - balAmt", fromBal, stakeOp.Id)

	if (exclusion + fromBal) < stakeOp.Amount {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	ledgerSession.AppendLedger(ledgerSystem.LedgerUpdate{
		Id:     stakeOp.Id,
		OpIdx:  0,
		Owner:  stakeOp.From,
		Amount: -stakeOp.Amount,
		Asset:  stakeOp.Asset,
		Type:   "stake",
		Memo:   stakeOp.Memo,
	})

	// le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], ledgerSystem.LedgerUpdate{
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

	// fee := int64(0)

	// if stakeOp.Instant {
	// 	fee = stakeOp.Amount * HBD_INSTANT_FEE / 100
	// 	if fee == 0 {
	// 		//Minimum of
	// 		fee = HBD_INSTANT_MIN
	// 	}
	// 	withdrawAmount := stakeOp.Amount - fee

	// 	ledgerSession.AppendLedger(ledgerSystem.LedgerUpdate{
	// 		Id:     stakeOp.Id + "#fee",
	// 		OpIdx:  0,
	// 		Owner:  HBD_FEE_RECEIVER,
	// 		Amount: fee,
	// 		Asset:  "hbd",
	// 		Type:   "fee",
	// 		Memo:   "HBD_INSTANT_FEE",
	// 	})
	// 	le.VirtualLedger[HBD_FEE_RECEIVER] = append(le.VirtualLedger[HBD_FEE_RECEIVER], ledgerSystem.LedgerUpdate{
	// 		Id:     stakeOp.Id + "#fee",
	// 		OpIdx:  0,
	// 		Owner:  HBD_FEE_RECEIVER,
	// 		Amount: fee,
	// 		Asset:  "hbd",
	// 		Type:   "fee",
	// 		Memo:   "HBD_INSTANT_FEE",
	// 	})
	// 	le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], ledgerSystem.LedgerUpdate{
	// 		Id:          stakeOp.Id,
	// 		BlockHeight: stakeOp.BlockHeight,
	// 		OpIdx:       0,
	// 		Owner:       stakeOp.To,
	// 		Amount:      withdrawAmount,
	// 		Asset:       "hbd_savings",
	// 		Type:        "stake",
	// 		Memo:        stakeOp.Memo,
	// 	})
	// }

	ledgerSession.AppendOplog(ledgerSystem.OpLogEvent{
		Id: stakeOp.Id,
		// Index: 1,

		From:   stakeOp.From,
		To:     stakeOp.To,
		Asset:  stakeOp.Asset,
		Amount: stakeOp.Amount,
		Memo:   stakeOp.Memo,
		Type:   "stake",

		// Params: map[string]interface{}{
		// 	"instant": stakeOp.Instant,
		// 	"fee":     fee,
		// },
	})

	return ledgerSystem.LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

func (le *LedgerExecutor) Unstake(stakeOp StakeOp, ledgerSession *LedgerSession) ledgerSystem.LedgerResult {
	//Cannot unstake less than 0.002 HBD
	//As there is a 0.001 HBD fee for instant staking
	if stakeOp.Amount <= 0 || (stakeOp.Instant && stakeOp.Amount < 2) {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	if stakeOp.Asset != "hbd" && stakeOp.Asset != "hbd_savings" {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "invalid asset",
		}
	}

	fromBal := ledgerSession.GetBalance(stakeOp.From, stakeOp.BlockHeight, "hbd_savings")

	le.Ls.log.Debug("Unstake - balAmt", fromBal, stakeOp.Amount)
	if fromBal < stakeOp.Amount {
		return ledgerSystem.LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	// fee := int64(0)

	ledgerSession.AppendLedger(ledgerSystem.LedgerUpdate{
		Id:     stakeOp.Id,
		OpIdx:  0,
		Owner:  stakeOp.From,
		Amount: -stakeOp.Amount,
		Asset:  stakeOp.Asset,
		Type:   "stake",
		Memo:   stakeOp.Memo,
	})
	// le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], )

	// if stakeOp.Instant {
	// 	//withdrawAmount < currently available "safe" unstaked balance
	// 	//enter in: some of kind of state tracking for "safe" unstaked balance

	// 	fee = stakeOp.Amount * HBD_INSTANT_FEE / 100

	// 	if fee == 0 {
	// 		//Minimum of
	// 		fee = HBD_INSTANT_MIN
	// 	}
	// 	withdrawAmount := stakeOp.Amount - fee

	// 	le.VirtualLedger[HBD_FEE_RECEIVER] = append(le.VirtualLedger[HBD_FEE_RECEIVER], ledgerSystem.LedgerUpdate{
	// 		Id:          stakeOp.Id + "#fee",
	// 		BlockHeight: stakeOp.BlockHeight,
	// 		OpIdx:       0,

	// 		Owner:  HBD_FEE_RECEIVER,
	// 		Amount: fee,
	// 		Asset:  "hbd",
	// 		Type:   "fee",
	// 		Memo:   "HBD_INSTANT_FEE",
	// 	})

	// 	le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], ledgerSystem.LedgerUpdate{
	// 		Id:          stakeOp.Id + "-out",
	// 		BlockHeight: stakeOp.BlockHeight,
	// 		OpIdx:       0,

	// 		Owner:  stakeOp.To,
	// 		Amount: withdrawAmount,
	// 		Asset:  "hbd",
	// 		Type:   "stake",
	// 		Memo:   stakeOp.Memo,
	// 	})
	// }

	ledgerSession.AppendOplog(ledgerSystem.OpLogEvent{
		Id: stakeOp.Id,
		// Index: 1,

		From:   stakeOp.From,
		To:     stakeOp.To,
		Asset:  stakeOp.Asset,
		Amount: stakeOp.Amount,
		Memo:   stakeOp.Memo,
		Type:   "unstake",

		// Params: map[string]interface{}{
		// 	"instant": stakeOp.Instant,
		// 	"fee":     fee,
		// },
	})

	return ledgerSystem.LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

func (le *LedgerExecutor) SnapshotForAccount(account string, blockHeight uint64, asset string) int64 {
	bal := le.Ls.GetBalance(account, blockHeight, asset)

	//le.Ls.log.Debug("getBalance le.VirtualLedger["+account+"]", le.VirtualLedger[account], blockHeight)

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
func FilterLedgerOps(query FilterLedgerParams, array []ledgerSystem.LedgerUpdate) []ledgerSystem.LedgerUpdate {
	outArray := make([]ledgerSystem.LedgerUpdate, 0)
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
