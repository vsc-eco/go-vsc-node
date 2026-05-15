package ledgerSystem

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	ledger_db "vsc-node/modules/db/vsc/ledger"
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

var log = vsclog.Module("ledger")

// Hive produces one block every 3 seconds → 365.25 * 24 * 3600 / 3 blocks per year.
const HIVE_BLOCKS_PER_YEAR = uint64(10_519_200)

type ledgerSystem struct {
	BalanceDb ledger_db.Balances
	LedgerDb  ledger_db.Ledger
	ClaimDb   ledger_db.InterestClaims

	//Bridge actions are side effects that require secondary (outside of execution) processing to complete
	//Some examples are withdrawals, and stake/unstake operations. Other future operations might be applicable as well
	//Anything that requires on chain processing to complete
	ActionsDb ledger_db.BridgeActions
}

const (
	// PendulumNodesHBDBucket is the single global HBD-only bucket holding the
	// cumulative node-runner share since the last epoch close. The swap-time
	// SDK method credits it; the epoch settlement op drains it. The bucket is
	// stored as the Owner field on each LedgerRecord; the asset column ("hbd")
	// is queried separately, so the constant stays asset-suffix-free.
	PendulumNodesHBDBucket = "pendulum:nodes"
)

func (ls *ledgerSystem) PendulumDistribute(toAccount string, amount int64, txID string, blockHeight uint64) LedgerResult {
	if strings.TrimSpace(toAccount) == "" {
		return LedgerResult{Ok: false, Msg: "invalid destination"}
	}
	if amount <= 0 {
		return LedgerResult{Ok: false, Msg: "invalid amount"}
	}
	if txID == "" {
		return LedgerResult{Ok: false, Msg: "missing tx id"}
	}
	available := ls.PendulumBucketBalance(PendulumNodesHBDBucket, blockHeight)
	if available < amount {
		return LedgerResult{Ok: false, Msg: "insufficient pendulum nodes bucket balance"}
	}
	ls.LedgerDb.StoreLedger(
		ledger_db.LedgerRecord{
			Id:          txID + "#distribute#" + toAccount,
			BlockHeight: blockHeight,
			Amount:      amount,
			Asset:       "hbd",
			From:        PendulumNodesHBDBucket,
			To:          toAccount,
			Type:        "pendulum_distribute",
		},
	)

	return LedgerResult{Ok: true, Msg: "success"}
}

// PendulumBucketBalance sums every ledger record whose Owner == bucket and
// Asset == hbd. The bucket is credited via paired LedgerSession.ExecuteTransfer
// records (type=transfer) at swap time and debited via PendulumDistribute
// records (type=pendulum_distribute) at settlement time.
func (ls *ledgerSystem) PendulumBucketBalance(bucket string, blockHeight uint64) int64 {
	if strings.TrimSpace(bucket) == "" {
		return 0
	}
	records, err := ls.LedgerDb.GetLedgerRange(bucket, 0, blockHeight, "hbd")
	if err != nil || records == nil {
		return 0
	}
	total := int64(0)
	for _, rec := range *records {
		total += rec.DeltaFor(bucket)
	}
	return total
}

func (ls *ledgerSystem) ClaimHBDInterest(lastClaim uint64, blockHeight uint64, amount int64, txId string) {
	log.Verbose("ClaimHBDInterest", "lastClaim", lastClaim, "bh", blockHeight, "amount", amount)
	//Do distribution of HBD interest on an going forward basis
	//Save to ledger DB the difference.
	ledgerBalances := ls.BalanceDb.GetAll(blockHeight)

	processedBalRecords := make([]ledger_db.BalanceRecord, 0)
	totalAvg := int64(0)
	//Ensure averages have been updated before distribution;
	for _, balance := range ledgerBalances {

		if blockHeight <= balance.HBD_CLAIM_HEIGHT {
			continue // Avoid underflow when claim height is at or beyond current block
		}
		B := blockHeight - balance.HBD_CLAIM_HEIGHT //Total blocks since last claim
		if B == 0 {
			continue //Avoid division by zero when claim height equals block height
		}
		if blockHeight <= balance.HBD_MODIFY_HEIGHT {
			continue // Avoid underflow when modify height is at or beyond current block
		}
		A := blockHeight - balance.HBD_MODIFY_HEIGHT //Blocks since last balance modification

		//HBD_AVG is an unnormalized cumulative sum (balance * blocks).
		//Add the current balance's contribution for the remaining period, then divide by B to get TWAB.
		endingAvg := (balance.HBD_AVG + balance.HBD_SAVINGS*int64(A)) / int64(B)

		if endingAvg < 1 {
			log.Verbose("ClaimHBD endingAvg sub-zero", "account", balance.Account, "avg", endingAvg)
			continue
		}

		balance.HBD_AVG = endingAvg

		processedBalRecords = append(processedBalRecords, balance)
		totalAvg = totalAvg + endingAvg
	}

	log.Verbose("processed bal records", "count", len(processedBalRecords), "totalAvg", totalAvg)

	for id, balance := range processedBalRecords {
		// if balance.HBD_AVG == 0 {
		// 	continue
		// }

		distributeAmt := balance.HBD_AVG * amount / totalAvg

		if distributeAmt > 0 {
			var owner string
			if strings.HasPrefix(balance.Account, "system:") {
				if balance.Account == "system:fr_balance" {
					owner = params.DAO_WALLET
				} else {
					//Filter
					continue
				}
			} else {
				owner = balance.Account
			}

			ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
				Id: "hbd_interest_" + strconv.Itoa(int(blockHeight)) + "_" + strconv.Itoa(id),
				//next block
				BlockHeight: blockHeight + 1,
				Amount:      int64(distributeAmt),
				Asset:       "hbd_savings",
				To:          owner,
				Type:        "interest",
			})
		}
	}
	var observedApr float64
	if totalAvg > 0 && lastClaim > 0 && blockHeight > lastClaim {
		blocksInPeriod := blockHeight - lastClaim
		observedApr = (float64(amount) / float64(totalAvg)) *
			(float64(HIVE_BLOCKS_PER_YEAR) / float64(blocksInPeriod))
	}

	savedAmount := amount
	if totalAvg == 0 {
		savedAmount = 0
	}
	ls.ClaimDb.SaveClaim(ledger_db.ClaimRecord{
		BlockHeight: blockHeight,
		Amount:      savedAmount,
		TxId:        txId,
		ReceivedN:   len(processedBalRecords),
		ObservedApr: observedApr,
	})
	//DONE
}

func (ls *ledgerSystem) IndexActions(actionUpdate ActionUpdate, extraInfo ExtraInfo) {
	log.Debug("IndexActions", "actionUpdate", actionUpdate)

	actionIds := actionUpdate.Ops

	//All stake related ops
	completeOps := actionUpdate.ClearedOps

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
			log.Debug("Indexxing stake Ledger")
			ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
				Id:     record.Id + ":hbd_savings",
				Amount: record.Amount,
				Asset:  "hbd_savings",
				To:     record.To,
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
				blockDelay = common.HBD_UNSTAKE_BLOCKS
			}
			ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
				Id:     record.Id + ":hbd",
				Amount: record.Amount,
				Asset:  "hbd",
				To:     record.To,
				Type:   "unstake",

				//It'll become available in 3 days of blocks
				BlockHeight: extraInfo.BlockHeight + blockDelay,
				BIdx:        -1,
				OpIdx:       -1,
			})
		}
	}

}

func (ls *ledgerSystem) IngestOplog(oplog []OpLogEvent, options OplogInjestOptions) {
	executeResults := ExecuteOplog(oplog, options.StartHeight, options.EndHeight)

	ledgerRecords := executeResults.ledgerRecords
	actionRecords := executeResults.actionRecords

	for _, v := range ledgerRecords {
		ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
			Id: v.Id,
			//plz passthrough original block height
			BlockHeight: options.EndHeight,
			Amount:      v.Amount,
			Asset:       v.Asset,
			From:        v.From,
			To:          v.To,
			Memo:        v.Memo,
			Params:      v.Params,
			Type:        v.Type,
		})
	}

	for _, v := range actionRecords {
		ls.ActionsDb.StoreAction(v)
	}

}

func (ls *ledgerSystem) GetBalance(account string, blockHeight uint64, asset string) int64 {
	lsz := ls.NewEmptyState()

	return lsz.GetBalance(account, blockHeight, asset)
}

// Used during live execution of transfers such as those from contracts or user transaction

// Empties the virtual state, such as when a block is executed

func (ls *ledgerSystem) Deposit(deposit Deposit) string {
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
	// if le.VirtualLedger == nil {
	// 	le.VirtualLedger = make(map[string][]LedgerUpdate)
	// }

	// if le.VirtualLedger[decodedParams.To] != nil {
	// 	le.Ls.log.Debug("ledgerExecutor", le.VirtualLedger[decodedParams.To])
	// }

	ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
		Id:          deposit.Id,
		BlockHeight: deposit.BlockHeight,
		Amount:      deposit.Amount,
		Asset:       deposit.Asset,
		To:          decodedParams.To,
		Type:        "deposit",
	})

	return decodedParams.To
}

func (ls *ledgerSystem) CalculationFractStats(accountList []string, blockHeight uint64) struct {
	//Total staked balance of hbd_savings accounts
	StakedBalance int64

	//Total fractional balance; must be staked
	FractionalBalance int64

	//Both numbers should add together to be equivalent to the total hbd savings of the vsc gateway wallet
} {

	//TODO: Make this work without needing to supply list of accounts.
	//Instead pull all accounts above >0 balance

	state := ls.NewEmptyState()

	hbdMap := make(map[string]int64)
	hbdSavingsMap := make(map[string]int64)

	for _, account := range accountList {
		hbdBal := state.SnapshotForAccount(account, blockHeight, "hbd")
		hbdSavingsBal := state.SnapshotForAccount(account, blockHeight, "hbd_savings")
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

	log.Verbose("balance distribution", "topCount", len(topBalances), "top5", topBal, "below5", belowBal)
	stakedAmt := belowBal / 3
	log.Verbose("staked amount computed", "stakedAmt", stakedAmt)

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

func (ls *ledgerSystem) NewEmptySession(state *LedgerState, startHeight uint64) LedgerSession {
	ledgerSession := ledgerSession{
		state: state,

		oplog:     make([]OpLogEvent, 0),
		ledgerOps: make([]LedgerUpdate, 0),
		balances:  make(map[string]*int64),
		idCache:   make(map[string]int),

		StartHeight: startHeight,
	}
	return &ledgerSession
}

func (ls *ledgerSystem) NewEmptyState() *LedgerState {
	return &LedgerState{
		Oplog:           make([]OpLogEvent, 0),
		VirtualLedger:   make(map[string][]LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		LedgerDb:        ls.LedgerDb,
		ActionDb:        ls.ActionsDb,
		BalanceDb:       ls.BalanceDb,
	}
}

// HBD instant stake/unstake fee

// Reserved for the future.
// Stake would trigger an indent to stake funds (immediately removing balance)
// Then trigger a delayed (actual stake) even when the onchain operation is executed through the gateway
// A two part Virtual Ledger operation operating out of sync

func New(balanceDb ledger_db.Balances, ledgerDb ledger_db.Ledger, claimDb ledger_db.InterestClaims, actionDb ledger_db.BridgeActions) LedgerSystem {
	return &ledgerSystem{
		BalanceDb: balanceDb,
		LedgerDb:  ledgerDb,
		ClaimDb:   claimDb,
		ActionsDb: actionDb,
	}
}
