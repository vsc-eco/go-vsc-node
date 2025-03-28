package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"vsc-node/lib/hive"
	"vsc-node/lib/logger"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"

	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
)

// VSC On chain gateway wallet
type MultiSig struct {
	identity      common.IdentityConfig
	ledgerActions ledgerDb.BridgeActions
	hiveCreator   hive.HiveTransactionCreator
	electionDb    elections.Elections
	witnessDb     witnesses.Witnesses
	balanceDb     ledgerDb.Balances
	VStream       *vstream.VStream

	service libp2p.PubSubService[p2pMessage]
	p2p     *libp2p.P2PServer
	se      *stateEngine.StateEngine
	log     logger.Logger
}

func (ms *MultiSig) Init() error {
	ms.VStream.RegisterBlockTick("multisig.tick", ms.BlockTick, false)
	return nil
}

func (ms *MultiSig) Start() *promise.Promise[any] {
	ms.startP2P()
	return utils.PromiseResolve[any](nil)
}

func (ms *MultiSig) Stop() error {
	ms.stopP2P()
	return nil
}

// var ROTATION_INTERVAL = uint64(20 * 60) //One hour of Hive blocks
var ROTATION_INTERVAL = uint64(20) //Test interval for e2e; TODO: Make this modifiable through env variables.
var ACTION_INTERVAL = uint64(20)   // One minute of Hive blocks
// var SYNC_INTERVAL = uint64(28_800) // One day of Hive blocks
var SYNC_INTERVAL = uint64(20) // Use during e2e testing

// Typically, the "richlist" of any system follows a disproportionate distribution
// Where the top 1% of accounts hold the majority of supply
// This ensures the sum of the top 5 balances are always kept liquid.
// Adjust this number as necessary
var HBD_TOP_CUTOFF = 5

func (ms *MultiSig) BlockTick(bh uint64) {
	if bh%ROTATION_INTERVAL == 0 || bh%ACTION_INTERVAL == 0 {

		ms.electionDb.GetElectionByHeight(bh)
		schedule := ms.se.GetSchedule(bh)
		slotInfo := stateEngine.CalculateSlotInfo(bh)

		var witnessSlot stateEngine.WitnessSlot
		for _, slot := range schedule {
			if slotInfo.EndHeight == slot.SlotHeight {
				witnessSlot = slot
				break
			}
		}

		if witnessSlot.Account != ms.identity.Get().HiveUsername {
			return
		}

		if bh%ROTATION_INTERVAL == 0 {
			go ms.TickKeyRotation(bh)
		}
		if bh%ACTION_INTERVAL == 0 {
			go ms.TickActions(bh)
		}
		if bh&SYNC_INTERVAL == 0 {
			go ms.SyncBalance(bh)
		}
	}
}

func (ms *MultiSig) TickKeyRotation(bh uint64) {
	electionResult, err := ms.electionDb.GetElectionByHeight(bh)

	if err != nil {
		return
	}

	gatewayKeys := make([][2]interface{}, 0)
	weightMap := make(map[string]uint64)
	var totalWeight uint64
	for idx, member := range electionResult.Members {
		witnessData, _ := ms.witnessDb.GetWitnessAtHeight(member.Account, &bh)
		var key [2]interface{}
		key[0] = witnessData.GatewayKey
		key[1] = 1
		gatewayKeys = append(gatewayKeys, key)

		weightMap[witnessData.GatewayKey] = electionResult.Weights[idx]
		totalWeight += electionResult.Weights[idx]
	}

	// len(gatewayKeys)

	var e [2]interface{}
	e[0] = "vsc.network"
	e[1] = 1

	jsonMetadata := map[string]interface{}{
		"msg":                 "Gateway wallet for the VSC Network",
		"website":             "https://vsc.network",
		"epcoh":               electionResult.Epoch,
		"last_block_rotation": bh,
	}

	jsonBytes, _ := json.Marshal(jsonMetadata)

	rotationTx := ms.hiveCreator.UpdateAccount(common.GATEWAY_WALLET, &hivego.Auths{
		WeightThreshold: int(totalWeight * 2 / 3),
		// AccountAuths:    weightMap,
		KeyAuths: gatewayKeys,
	}, nil, &hivego.Auths{
		WeightThreshold: 1,
		AccountAuths: [][2]any{
			e,
		},
	}, string(jsonBytes), "STM8buQNWovTcX7H8yLdYNx82xDddQE9R5MzQDNg4mocScnXTGSkE")

	tx := ms.hiveCreator.MakeTransaction([]hivego.HiveOperation{rotationTx})
	ms.hiveCreator.PopulateSigningProps(&tx, nil)

	ms.hiveCreator.Broadcast(tx)
}

func (ms *MultiSig) TickActions(bh uint64) {
	actions, err := ms.ledgerActions.GetPendingActions(bh)

	if err != nil {
		return
	}
	if len(actions) == 0 {
		return
	}

	ops := []hivego.HiveOperation{}
	stakeBal := uint64(0)
	unstakeBal := uint64(0)
	stakeTxCount := 0
	unstakeTxCount := 0
	executedOps := make([]string, 0)
	for _, action := range actions {
		// ops = append(ops, ms.createWithdrawOps(action)...)

		executedOps = append(executedOps, action.Id)
		if action.Type == "withdraw" {
			splitTo := strings.Split(action.To, ":")
			net := splitTo[0]
			to := splitTo[1]

			//Safety protection against bad inputs if other protections fail
			if net != "hive" {
				continue
			}
			amt := action.Amount

			op := ms.hiveCreator.Transfer(common.GATEWAY_WALLET, to, hive.AmountToString(amt), strings.ToUpper(action.Asset), "Withdrawal from vsc.network")

			ops = append(ops, op)
		}

		if action.Type == "stake" {
			stakeBal += uint64(action.Amount)
			stakeTxCount += 1
		}
		if action.Type == "unstake" {
			unstakeBal += uint64(action.Amount)
			unstakeTxCount += 1
		}
	}

	if stakeBal > unstakeBal {
		//Must stake
		mustStakeBal := int64(stakeBal - unstakeBal)

		amtStr := hive.AmountToString(mustStakeBal)

		op := ms.hiveCreator.TransferToSavings(common.GATEWAY_WALLET, common.GATEWAY_WALLET, amtStr, "HBD", "Staking "+amtStr+" HBD from "+strconv.Itoa(stakeTxCount)+" transactions")

		ops = append(ops, op)
	} else if unstakeBal > stakeBal {
		//Must unstake
		mustUnstakeBal := int64(unstakeBal - stakeBal)

		amtStr := hive.AmountToString(mustUnstakeBal)

		op := ms.hiveCreator.TransferFromSavings(common.GATEWAY_WALLET, common.GATEWAY_WALLET, amtStr, "HBD", "Unstaking "+amtStr+" HBD from "+strconv.Itoa(unstakeTxCount)+" transactions", int(bh))

		ops = append(ops, op)
	}

	//Stake Ops of any category
	unstakeOps := make([]ledgerDb.ActionRecord, 0)

	for _, action := range actions {
		if action.Type == "unstake" && action.Asset == "hbd" {
			unstakeOps = append(unstakeOps, action)
		}
	}

	slices.SortFunc(
		unstakeOps,
		func(a ledgerDb.ActionRecord, b ledgerDb.ActionRecord) int {
			return int(a.Amount) - int(b.Amount)
		},
	)

	clearedOps := make([]string, 0)

	clearedBal := uint64(0)
	for _, action := range unstakeOps {
		clearedBal += uint64(action.Amount)

		if clearedBal > unstakeBal {
			break
		}
		clearedOps = append(clearedOps, action.Id)
	}

	bs := big.Int{}

	for idx, id := range executedOps {
		if slices.Contains(clearedOps, id) {
			bs.SetBit(&bs, idx, 1)
		}
	}

	bsBytes := bs.Bytes()

	b64Bytes := base64.RawStdEncoding.EncodeToString(bsBytes)

	actionHeader := ChainAction{
		Ops:        executedOps,
		ClearedOps: b64Bytes,
	}

	fmt.Println("Prefix: e2e-1]: ChainAction.ClearedOps="+b64Bytes, bsBytes)

	headerBytes, _ := json.Marshal(actionHeader)
	headerStr := string(headerBytes)

	headerOp := ms.hiveCreator.CustomJson([]string{common.GATEWAY_WALLET}, []string{}, "vsc.actions", headerStr)

	ops = append([]hivego.HiveOperation{headerOp}, ops...)
	tx := ms.hiveCreator.MakeTransaction(ops)

	ms.hiveCreator.PopulateSigningProps(&tx, nil)

	txId, _ := ms.hiveCreator.Broadcast(tx)

	ms.log.Debug("TickAction TxID!", txId, tx)

	//Do signing
}

// Executes on chain actions such as withdrawals, staking, unstaking, etc
func (ms *MultiSig) ExecuteActions() {

}

// Automatic function to claim HBD interest
func (ms *MultiSig) ClaimHBDInterest() {

}

// Handles incoming signing request
func (ms *MultiSig) HandleClaimHbdInterest() {

}

// Sync balances between liquid and staked HBD
func (ms *MultiSig) SyncBalance(bh uint64) {
	//system:sync_balance is the tag that is used to track the system balance
	//Note: when interest is claimed it goes to a separate account
	//This account is considered a "virtual" account.
	balRecord, _ := ms.balanceDb.GetBalanceRecord("system:fr_balance", bh)

	stakedBal := int64(0)
	if balRecord != nil {
		stakedBal = balRecord.HBD_SAVINGS
	}

	totalHbd := int64(0)
	balList := make(map[string]int64, 0)
	balRecords := ms.balanceDb.GetAll(bh)
	topBalances := make([]int64, 0)
	for _, record := range balRecords {
		//Don't include any system balances
		if !strings.HasPrefix(record.Account, "system:") {
			totalHbd += int64(record.HBD)
			balList[record.Account] = record.HBD

			topBalances = append(topBalances, int64(record.HBD))

			fmt.Println("syncBalance - appending", record.Account, record.HBD)
		}
	}
	sort.Slice(topBalances, func(i, j int) bool {
		return topBalances[i] > topBalances[j]
	})

	if len(topBalances) < 6 {

		fmt.Println("syncBalance conditions not met", topBalances, totalHbd, stakedBal)
		return
	}

	topBals := topBalances[:HBD_TOP_CUTOFF]
	belowBals := topBalances[HBD_TOP_CUTOFF:]

	topBal := int64(0)
	belowBal := int64(0)
	for _, v := range topBals {
		topBal = topBal + v
	}

	for _, v := range belowBals {
		belowBal = belowBal + v
	}

	//1/3 of the majority accounts
	stakeAmt := belowBal / 3

	var hbdToStake int64
	var hbdToUnstake int64
	if stakeAmt > stakedBal {
		hbdToStake = stakeAmt - stakedBal
	} else if stakeAmt < stakedBal {
		hbdToUnstake = stakedBal - stakeAmt
	}

	//100.000 HBD to the minimum amount to unstake
	//Or stakedBal is under 150 HBD
	//Adjust minimums as necessary
	var ops []hivego.HiveOperation
	if (hbdToStake > 100_000 || stakedBal < 150_000) && hbdToStake != 0 {
		op := ms.hiveCreator.TransferToSavings(common.GATEWAY_WALLET, common.GATEWAY_WALLET, hive.AmountToString(hbdToStake), "HBD", "Staking "+hive.AmountToString(hbdToStake)+" HBD")

		ops = append(ops, op)
	} else if (hbdToUnstake > 10_000 || stakedBal < 10_000) && hbdToUnstake != 0 {
		op := ms.hiveCreator.TransferFromSavings(common.GATEWAY_WALLET, common.GATEWAY_WALLET, hive.AmountToString(hbdToStake), "HBD", "Staking "+hive.AmountToString(hbdToStake)+" HBD", int(bh+1))

		ops = append(ops, op)

	}

	if len(ops) > 0 {
		header := map[string]interface{}{
			"stake_amt":   hbdToStake,
			"unstake_amt": hbdToUnstake,
		}

		headerBytes, _ := json.Marshal(header)

		headerOp := ms.hiveCreator.CustomJson([]string{common.GATEWAY_WALLET}, []string{}, "vsc.fr_sync", string(headerBytes))

		ops = append([]hivego.HiveOperation{headerOp}, ops...)

		tx := ms.hiveCreator.MakeTransaction(ops)

		ms.hiveCreator.PopulateSigningProps(&tx, nil)

		txId, _ := ms.hiveCreator.Broadcast(tx)

		fmt.Println("Prefix: e2e-1]: SyncBalance TxID!", txId, tx)
		// return ops
	}

}

var _ a.Plugin = &MultiSig{}

func New(logger logger.Logger, witnessDb witnesses.Witnesses, electionDb elections.Elections, ledgerActions ledgerDb.BridgeActions, balanceDb ledgerDb.Balances, hiveCreator hive.HiveTransactionCreator, vstream *vstream.VStream, p2p *libp2p.P2PServer, se *stateEngine.StateEngine, identityConfig common.IdentityConfig) *MultiSig {
	return &MultiSig{
		witnessDb:     witnessDb,
		electionDb:    electionDb,
		ledgerActions: ledgerActions,
		balanceDb:     balanceDb,
		hiveCreator:   hiveCreator,
		VStream:       vstream,
		p2p:           p2p,
		se:            se,
		identity:      identityConfig,
		log:           logger,
	}
}
