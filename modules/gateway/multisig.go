package gateway

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
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
	hiveClient    *hivego.HiveRpcNode
	electionDb    elections.Elections
	witnessDb     witnesses.Witnesses
	balanceDb     ledgerDb.Balances
	VStream       *vstream.VStream

	service libp2p.PubSubService[p2pMessage]
	p2p     *libp2p.P2PServer
	se      *stateEngine.StateEngine
	log     logger.Logger
	msgChan map[string]chan p2pMessage

	bh uint64
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

var ROTATION_INTERVAL = uint64(20 * 60) //One hour of Hive blocks
// var ROTATION_INTERVAL = uint64(20) //Test interval for e2e; TODO: Make this modifiable through env variables.
var ACTION_INTERVAL = uint64(20)   // One minute of Hive blocks
var SYNC_INTERVAL = uint64(28_800) // One day of Hive blocks
// var SYNC_INTERVAL = uint64(20) // Use during e2e testing

func (ms *MultiSig) BlockTick(bh uint64, headHeight uint64) {
	ms.bh = bh

	if bh < headHeight-20 {
		return
	}
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

		if bh%20 == 0 {
			go ms.TickKeyRotation(bh)
		}
		if bh%ACTION_INTERVAL == 0 {
			go ms.TickActions(bh)
		}
		if bh&SYNC_INTERVAL == 0 {
			go ms.TickSyncFr(bh)
		}
	}
}

func (ms *MultiSig) TickKeyRotation(bh uint64) {
	signPkg, err := ms.keyRotation(bh)

	if err != nil {
		return
	}

	ms.msgChan[signPkg.TxId] = make(chan p2pMessage)
	signReq := signRequest{
		TxId:        signPkg.TxId,
		BlockHeight: bh,
	}

	sigJson, _ := json.Marshal(signReq)

	ms.service.Send(p2pMessage{
		Type: "sign_request",
		Op:   "key_rotation",
		Data: string(sigJson),
	})

	threshold, _, _, _ := ms.getThreshold()
	signatures, weight, err := ms.waitForSigs(signPkg.Tx, signPkg.TxId)

	if err != nil {
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weight == uint64(threshold) {
		rotationId, _ := ms.hiveCreator.Broadcast(tx)

		fmt.Println("Rotation txId", rotationId)
	}
}

func (ms *MultiSig) TickActions(bh uint64) {
	signPkg, err := ms.executeActions(bh)

	if err != nil {
		return
	}

	ms.msgChan[signPkg.TxId] = make(chan p2pMessage)
	signReq := signRequest{
		TxId:        signPkg.TxId,
		BlockHeight: bh,
	}

	sigJson, _ := json.Marshal(signReq)

	ms.service.Send(p2pMessage{
		Type: "sign_request",
		Op:   "execute_actions",
		Data: string(sigJson),
	})

	threshold, _, _, _ := ms.getThreshold()
	signatures, weight, err := ms.waitForSigs(signPkg.Tx, signPkg.TxId)

	if err != nil {
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weight == uint64(threshold) {
		rotationId, _ := ms.hiveCreator.Broadcast(tx)

		fmt.Println("Actions txId", rotationId)
	}
}

func (ms *MultiSig) TickSyncFr(bh uint64) {

	signPkg, err := ms.syncBalance(bh)

	if err != nil {
		return
	}

	ms.msgChan[signPkg.TxId] = make(chan p2pMessage)
	signReq := signRequest{
		TxId:        signPkg.TxId,
		BlockHeight: bh,
	}

	sigJson, _ := json.Marshal(signReq)

	ms.service.Send(p2pMessage{
		Type: "sign_request",
		Op:   "fr_sync",
		Data: string(sigJson),
	})

	threshold, _, _, _ := ms.getThreshold()
	signatures, weight, err := ms.waitForSigs(signPkg.Tx, signPkg.TxId)

	if err != nil {
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weight == uint64(threshold) {
		rotationId, _ := ms.hiveCreator.Broadcast(tx)

		fmt.Println("Actions txId", rotationId)
	}
}

func (ms *MultiSig) keyRotation(bh uint64) (signingPackage, error) {
	if bh%ACTION_INTERVAL != 0 {
		return signingPackage{}, errors.New("invalid slot")
	}
	electionResult, err := ms.electionDb.GetElectionByHeight(bh)

	if err != nil {
		return signingPackage{}, err
	}

	weightMap := make(map[string]uint64)
	gatewayKeys := make([][2]interface{}, 0)
	for idx, member := range electionResult.Members {
		witnessData, _ := ms.witnessDb.GetWitnessAtHeight(member.Account, &bh)
		if witnessData == nil {
			fmt.Println("No witness data for", member.Account, "at", bh)
			continue
		}
		var key [2]interface{}
		key[0] = witnessData.GatewayKey
		key[1] = 1
		gatewayKeys = append(gatewayKeys, key)

		weightMap[witnessData.GatewayKey] = electionResult.Weights[idx]
	}

	slices.SortFunc(gatewayKeys, func(a, b [2]interface{}) int {
		aKey := a[0].(string)
		bKey := b[0].(string)
		return int(weightMap[aKey]) - int(weightMap[bKey])
	})

	cutOff := 0
	if len(gatewayKeys) > 40 {
		cutOff = 40
	} else {
		cutOff = len(gatewayKeys)
	}
	gatewayKeys = gatewayKeys[:cutOff]

	if len(gatewayKeys) < 8 {
		return signingPackage{}, errors.New("not enough keys")
	}

	var e [2]interface{}
	e[0] = "vsc.dao"
	e[1] = 1

	totalWeight := len(gatewayKeys)
	weightThreshold := int(totalWeight * 2 / 3)

	var o [2]interface{}
	o[0] = "vsc.dao"
	o[1] = weightThreshold

	jsonMetadata := map[string]interface{}{
		"msg":                 "Gateway wallet for the VSC Network",
		"website":             "https://vsc.network",
		"epcoh":               electionResult.Epoch,
		"last_block_rotation": bh,
	}

	jsonBytes, _ := json.Marshal(jsonMetadata)

	rotationTx := ms.hiveCreator.UpdateAccount(common.GATEWAY_WALLET, &hivego.Auths{
		WeightThreshold: weightThreshold,
		// AccountAuths:    weightMap,
		KeyAuths: gatewayKeys,
		AccountAuths: [][2]any{
			o,
		},
	}, nil, &hivego.Auths{
		WeightThreshold: 1,
		AccountAuths: [][2]any{
			e,
		},
	}, string(jsonBytes), "STM8buQNWovTcX7H8yLdYNx82xDddQE9R5MzQDNg4mocScnXTGSkE")

	tx := ms.hiveCreator.MakeTransaction([]hivego.HiveOperation{rotationTx})
	ms.hiveCreator.PopulateSigningProps(&tx, []int{int(bh)})

	txId, _ := tx.GenerateTrxId()

	return signingPackage{
		Ops:  []hivego.HiveOperation{rotationTx},
		Tx:   tx,
		TxId: txId,
	}, nil
}

func (ms *MultiSig) executeActions(bh uint64) (signingPackage, error) {
	if bh%ACTION_INTERVAL != 0 {
		return signingPackage{}, errors.New("invalid slot")
	}
	actionFilter := []string{
		"withdraw", "stake", "unstake",
	}
	actions, err := ms.ledgerActions.GetPendingActions(bh, actionFilter...)

	// fmt.Println("Tick actions", actions, err)
	if err != nil {
		return signingPackage{}, err
	}
	if len(actions) == 0 {
		return signingPackage{}, errors.New("no actions to process")
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

	ms.hiveCreator.PopulateSigningProps(&tx, []int{int(bh)})

	txId, _ := tx.GenerateTrxId()

	//Do signing

	return signingPackage{
		Ops:  ops,
		Tx:   tx,
		TxId: txId,
	}, nil
}

// Sync balances between liquid and staked HBD
func (ms *MultiSig) syncBalance(bh uint64) (signingPackage, error) {
	if bh%ACTION_INTERVAL != 0 {
		return signingPackage{}, errors.New("invalid slot")
	}
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

		return signingPackage{}, errors.New("no sync to process")
	}

	totalBal := int64(0)
	for _, bal := range topBalances {
		totalBal = totalBal + bal
	}

	//1/3 of the majority accounts
	stakeAmt := totalBal / 3

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

		ms.hiveCreator.PopulateSigningProps(&tx, []int{int(bh)})

		txId, _ := tx.GenerateTrxId()

		return signingPackage{
			Ops:  ops,
			Tx:   tx,
			TxId: txId,
		}, nil
		// return ops
	}

	return signingPackage{}, errors.New("no sync to process")
}

// Automatic function to claim HBD interest
func (ms *MultiSig) ClaimHBDInterest() {

}

func (ms *MultiSig) getThreshold() (int, []string, []int, error) {
	accountData, err := ms.hiveClient.GetAccount([]string{common.GATEWAY_WALLET})

	if err != nil {
		return 0, nil, nil, err
	}
	if len(accountData) == 0 {
		return 0, nil, nil, errors.New("account not found")
	}
	gatewayAccount := accountData[0]

	publicKeys := make([]string, 0)
	weights := make([]int, 0)
	for _, key := range gatewayAccount.Owner.KeyAuths {
		publicKeys = append(publicKeys, key[0].(string))
		weights = append(weights, int(key[1].(float64)))
	}

	return gatewayAccount.Owner.WeightThreshold, publicKeys, weights, nil
}

func (ms *MultiSig) waitForSigs(tx hivego.HiveTransaction, hivetxId string) ([]string, uint64, error) {
	threshold, publicList, weights, _ := ms.getThreshold()
	txId, err := tx.GenerateTrxId()
	if err != nil {
		return nil, 0, err
	}
	if ms.msgChan[txId] == nil {
		return nil, 0, errors.New("no channel for txId")
	}

	txBytes, err := hivego.SerializeTx(tx)

	if err != nil {
		return nil, 0, err
	}
	txHash := hivego.HashTxForSig(txBytes)

	signedWeight := uint64(0)
	sigs := make([]string, 0)
	for uint64(threshold) > signedWeight {
		msg := <-ms.msgChan[txId]
		if msg.Type == "sign_response" {
			sigRes := signResponse{}
			err := json.Unmarshal([]byte(msg.Data), &sigRes)

			if err == nil {

				pubKey, err := recoverPublicKey(sigRes.Sig, txHash)
				if err != nil {
					return nil, 0, err
				}
				idx := slices.Index(publicList, pubKey)
				if idx != -1 {
					sigs = append(sigs, sigRes.Sig)
					signedWeight = signedWeight + uint64(weights[idx])
				}
			}
		}
	}

	return sigs, signedWeight, nil
}

func (ms *MultiSig) waitCheckBh(INTERVAL uint64, blockHeight uint64) error {
	if blockHeight%INTERVAL != 0 {
		return errors.New("invalid interval")
	}

	if blockHeight > ms.bh {
		//if too far into future!
		if blockHeight-10 > ms.bh {
			return nil
		}

		var clear bool
		for i := 0; i < 20; i++ {
			if ms.bh == blockHeight {
				clear = true
				break
			}
			time.Sleep(time.Second)
		}
		if !clear {
			return errors.New("timeout waiting for block height")
		}
	} else if blockHeight < ms.bh-10 {
		return errors.New("too far into past")
	}

	return nil
}

var _ a.Plugin = &MultiSig{}

func New(logger logger.Logger, witnessDb witnesses.Witnesses, electionDb elections.Elections, ledgerActions ledgerDb.BridgeActions, balanceDb ledgerDb.Balances, hiveCreator hive.HiveTransactionCreator, vstream *vstream.VStream, p2p *libp2p.P2PServer, se *stateEngine.StateEngine, identityConfig common.IdentityConfig, hiveClient *hivego.HiveRpcNode) *MultiSig {
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
		hiveClient:    hiveClient,

		msgChan: make(map[string]chan p2pMessage),
	}
}
