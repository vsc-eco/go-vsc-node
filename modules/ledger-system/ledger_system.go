package ledgerSystem

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"vsc-node/lib/logger"
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

type ledgerSystem struct {
	BalanceDb ledger_db.Balances
	LedgerDb  ledger_db.Ledger
	ClaimDb   ledger_db.InterestClaims

	//Bridge actions are side effects that require secondary (outside of execution) processing to complete
	//Some examples are withdrawals, and stake/unstake operations. Other future operations might be applicable as well
	//Anything that requires on chain processing to complete
	ActionsDb ledger_db.BridgeActions

	log logger.Logger
}

func (ls *ledgerSystem) ClaimHBDInterest(lastClaim uint64, blockHeight uint64, amount int64) {
	fmt.Println("ClaimHBDInterest", lastClaim, blockHeight, amount)
	//Do distribution of HBD interest on an going forward basis
	//Save to ledger DB the difference.
	ledgerBalances := ls.BalanceDb.GetAll(blockHeight)

	processedBalRecords := make([]ledger_db.BalanceRecord, 0)
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
				Owner:       owner,
				Type:        "interest",
			})
		}
	}
	//Note this calculation is inaccurate and should be calculated based on N blocks of Y claim period
	//Should not assume a static amount of time or 12 exactly claim interals per year. But since this is for statistics, it doesn't matter.
	observedApr := (float64(amount) / float64(totalAvg)) * 12

	ls.ClaimDb.SaveClaim(ledger_db.ClaimRecord{
		BlockHeight: blockHeight,
		Amount:      amount,
		ReceivedN:   len(processedBalRecords),
		ObservedApr: observedApr,
	})
	//DONE
}

func (ls *ledgerSystem) IndexActions(actionUpdate map[string]interface{}, extraInfo ExtraInfo) {
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
			ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
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
				blockDelay = common.HBD_UNSTAKE_BLOCKS
			}
			ls.LedgerDb.StoreLedger(ledger_db.LedgerRecord{
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
			Owner:       v.Owner,
			Type:        v.Type,
			// TxId:        v.,
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
		From:        deposit.From,
		Owner:       decodedParams.To,
		Type:        "deposit",
		TxId:        deposit.Id,
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

func New(balanceDb ledger_db.Balances, ledgerDb ledger_db.Ledger, claimDb ledger_db.InterestClaims, actionDb ledger_db.BridgeActions, log logger.Logger) LedgerSystem {
	return &ledgerSystem{
		BalanceDb: balanceDb,
		LedgerDb:  ledgerDb,
		ClaimDb:   claimDb,
		ActionsDb: actionDb,
		log:       log,
	}
}
