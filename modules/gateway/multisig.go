package gateway

import (
	"cmp"
	"context"
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
	"vsc-node/lib/utils"
	"vsc-node/lib/vsclog"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
)

var log = vsclog.Module("gateway")

// VSC On chain gateway wallet
// hiveAccountClient is the slice of *hivego.HiveRpcNode that getThreshold
// needs. Extracted to an interface (review2 HIGH #80) so the
// getThreshold-error path is unit-testable; *hivego.HiveRpcNode satisfies it,
// so wiring is behaviour-neutral. (hiveClient stays a concrete pointer for
// p2p.go's hiveClient.ChainID field access.)
type hiveAccountClient interface {
	GetAccount(accountNames []string) ([]hivego.AccountData, error)
}

type MultiSig struct {
	sconf         systemconfig.SystemConfig
	identity      common.IdentityConfig
	ledgerActions ledgerDb.BridgeActions
	hiveCreator   hive.HiveTransactionCreator
	hiveClient    *hivego.HiveRpcNode
	accountClient hiveAccountClient
	electionDb    elections.Elections
	witnessDb     witnesses.Witnesses
	balanceDb     ledgerDb.Balances
	hiveConsumer  *blockconsumer.HiveConsumer

	service libp2p.PubSubService[p2pMessage]
	p2p     *libp2p.P2PServer
	se      *stateEngine.StateEngine
	msgChan map[string]chan *p2pMessage

	bh        uint64
	pendingTx *pendingGatewayTx
}

func (ms *MultiSig) Init() error {
	ms.hiveConsumer.RegisterBlockTick("multisig.tick", ms.BlockTick, false)
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
var ACTION_INTERVAL = uint64(20) // One minute of Hive blocks
var SYNC_INTERVAL = uint64(7200) // Every 6 hours
// var SYNC_INTERVAL = uint64(20) // Use during e2e testing

func (ms *MultiSig) BlockTick(bh uint64, headHeight *uint64) {
	ms.bh = bh
	if headHeight == nil {
		return
	}
	// review2 LOW #112: *headHeight-20 underflows to ~1.8e19 when the head
	// is below block 20 (fresh chain), making this skip every early block.
	// bh+20 < *headHeight is the same predicate without the underflow.
	if bh+20 < *headHeight {
		return
	}

	if bh%ROTATION_INTERVAL == 0 || bh%ACTION_INTERVAL == 0 {

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
			fmt.Println("Multisig: Running key rotation")
			go ms.TickKeyRotation(bh)
		}
		if bh%ACTION_INTERVAL == 0 {
			fmt.Println("Multisig: Running Actions")
			go ms.TickActions(bh)
		}
		if bh%SYNC_INTERVAL == 0 {
			go ms.TickSyncFr(bh)
		}
	}
}

func (ms *MultiSig) TickKeyRotation(bh uint64) {
	signPkg, err := ms.keyRotation(bh)

	if err != nil {
		return
	}

	ms.msgChan[signPkg.TxId] = make(chan *p2pMessage, 16)
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

	threshold, _, _, thErr := ms.getThreshold()
	if thErr != nil || threshold <= 0 {
		// review2 HIGH #80: never proceed with threshold==0 — it makes the
		// later `weight == threshold` gate trivially true and broadcasts an
		// under-signed transaction.
		fmt.Println("keyRotation getThreshold failed, aborting", thErr, threshold)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	signatures, weight, err := ms.waitForSigs(ctx, signPkg.Tx, signPkg.TxId)
	delete(ms.msgChan, signPkg.TxId)

	if err != nil {
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weight == uint64(threshold) {
		rotationId, err := ms.hiveCreator.Broadcast(tx)

		fmt.Println("Rotation txId", rotationId, err)
	}
}

// pendingTxProcessed returns true once every action in the stored pending
// tx has been marked "complete" by the state engine — i.e. the L1
// `vsc.actions` header for this batch has been re-ingested and
// IndexActions -> ExecuteComplete fired. At that point the stored tx is
// safe to prune.
func (ms *MultiSig) pendingTxProcessed() bool {
	if ms.pendingTx == nil {
		return false
	}
	for _, id := range ms.pendingTx.ActionIds {
		rec, err := ms.ledgerActions.Get(id)
		if err != nil || rec == nil {
			continue
		}
		if rec.Status != "complete" {
			return false
		}
	}
	return true
}

func (ms *MultiSig) prunePendingTx() {
	ms.pendingTx = nil
}

func (ms *MultiSig) TickActions(bh uint64) {
	// If we have a pending broadcast from a previous tick, either prune
	// it (state engine confirmed every action complete), revert + prune
	// (the Hive tx's expiration window has passed without confirmation —
	// 10 blocks ≈ 30s, matches the Hive tx wall-clock expiration set in
	// PopulateSigningProps), or retry the broadcast (idempotent — Hive
	// nodes reject duplicates by txid). Until pendingTx is cleared we do
	// NOT build a new batch, so we cannot double-broadcast on top of a
	// still-alive signed tx.
	if ms.pendingTx != nil {
		if ms.pendingTxProcessed() {
			fmt.Println("TickActions: pending tx processed by state engine, pruning")
			ms.prunePendingTx()
			return
		}

		if bh > ms.pendingTx.ExpirationBlock {
			fmt.Println("TickActions: pending tx expired, reverting actions and pruning")
			ms.ledgerActions.RevertToPending(ms.pendingTx.ActionIds...)
			ms.prunePendingTx()
			return
		}

		fmt.Println("TickActions: retrying broadcast of pending tx", ms.pendingTx.TxId)
		_, retryErr := ms.hiveCreator.Broadcast(ms.pendingTx.SignedTx)
		fmt.Println("TickActions: retry result", retryErr)
		return
	}

	signPkg, err := ms.executeActions(bh)

	fmt.Println("TickActions", err, signPkg)
	if err != nil {
		return
	}

	ms.msgChan[signPkg.TxId] = make(chan *p2pMessage, 16)
	signReq := signRequest{
		TxId:        signPkg.TxId,
		BlockHeight: bh,
	}

	sigJson, _ := json.Marshal(signReq)

	go func() {
		time.Sleep(5 * time.Millisecond)
		ms.service.Send(p2pMessage{
			Type: "sign_request",
			Op:   "execute_actions",
			Data: string(sigJson),
		})
	}()

	fmt.Println("TickActions getThreshold()")
	threshold, _, _, thErr := ms.getThreshold()
	if thErr != nil || threshold <= 0 {
		// review2 HIGH #80: see keyRotation — abort instead of broadcasting
		// an under-signed actions transaction. Revert the local
		// SetProcessing transition so the actions are retriable on the
		// next ACTION_INTERVAL tick instead of being stuck "processing"
		// forever (cosigner split-brain follow-up: a getThreshold blip
		// here used to permanently strand the batch on the leader).
		fmt.Println("TickActions getThreshold failed, aborting", thErr, threshold)
		ms.ledgerActions.RevertToPending(signPkg.ExecutedOps...)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	signatures, weight, err := ms.waitForSigs(ctx, signPkg.Tx, signPkg.TxId)
	delete(ms.msgChan, signPkg.TxId)

	fmt.Println("TickActions signatures", signatures, weight, err)
	if err != nil {
		ms.ledgerActions.RevertToPending(signPkg.ExecutedOps...)
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weight < uint64(threshold) {
		ms.ledgerActions.RevertToPending(signPkg.ExecutedOps...)
		return
	}

	_, broadcastErr := ms.hiveCreator.Broadcast(tx)
	fmt.Println("Actions txId", signPkg.TxId, broadcastErr)

	ms.pendingTx = &pendingGatewayTx{
		SignedTx:        tx,
		TxId:            signPkg.TxId,
		ActionIds:       signPkg.ExecutedOps,
		Expiration:      tx.Expiration,
		ExpirationBlock: bh + 10,
	}
}

func (ms *MultiSig) TickSyncFr(bh uint64) {

	signPkg, err := ms.syncBalance(bh)

	fmt.Println("signPkg, err", signPkg, err)
	if err != nil {
		return
	}

	ms.msgChan[signPkg.TxId] = make(chan *p2pMessage, 16)
	signReq := signRequest{
		TxId:        signPkg.TxId,
		BlockHeight: bh,
	}

	sigJson, _ := json.Marshal(signReq)

	go func() {
		time.Sleep(5 * time.Millisecond)
		ms.service.Send(p2pMessage{
			Type: "sign_request",
			Op:   "fr_sync",
			Data: string(sigJson),
		})
	}()

	threshold, _, _, thErr := ms.getThreshold()
	if thErr != nil || threshold <= 0 {
		// review2 HIGH #80: see keyRotation — abort instead of broadcasting
		// an under-signed fr_sync transaction.
		fmt.Println("TickSyncFr getThreshold failed, aborting", thErr, threshold)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	signatures, weight, err := ms.waitForSigs(ctx, signPkg.Tx, signPkg.TxId)
	delete(ms.msgChan, signPkg.TxId)

	if err != nil {
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weight == uint64(threshold) {
		rotationId, err := ms.hiveCreator.Broadcast(tx)

		fmt.Println("SyncFr txId", rotationId, err)
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
		if witnessData.GatewayKey == "" {
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
		// review2 MEDIUM #57: int(uint64) truncates for weights >= 2^63
		// and int(a)-int(b) overflows for large gaps, producing a wrong
		// (or non-transitive) gateway-key order. Compare the uint64
		// weights directly.
		return cmp.Compare(weightMap[aKey], weightMap[bKey])
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

	var eb [2]interface{}
	eb[0] = "vsc.network"
	eb[1] = 1

	totalWeight := len(gatewayKeys)
	// review2 HIGH #29: ceil(2N/3), not floor — floor let a sub-2/3 set of
	// gateway signers move funds.
	weightThreshold := gatewayWeightThreshold(totalWeight)

	var o [2]interface{}
	o[0] = "vsc.dao"
	o[1] = weightThreshold

	jsonMetadata := map[string]interface{}{
		"msg":                 "Gateway wallet for the VSC Network",
		"website":             "https://vsc.network",
		"epoch":               electionResult.Epoch,
		"last_block_rotation": bh,
	}

	jsonBytes, _ := json.Marshal(jsonMetadata)

	rotationTx := ms.hiveCreator.UpdateAccount(ms.sconf.GatewayWallet(), &hivego.Auths{
		WeightThreshold: weightThreshold,
		// AccountAuths:    weightMap,
		KeyAuths: gatewayKeys,
		AccountAuths: [][2]any{
			o,
		},
	}, &hivego.Auths{
		WeightThreshold: weightThreshold,
		KeyAuths:        gatewayKeys,
		AccountAuths:    [][2]any{},
	}, &hivego.Auths{
		WeightThreshold: 1,
		AccountAuths: [][2]any{
			e,
			eb,
		},
		KeyAuths: [][2]any{},
	}, string(jsonBytes), "STM8buQNWovTcX7H8yLdYNx82xDddQE9R5MzQDNg4mocScnXTGSkE")

	tx := ms.hiveCreator.MakeTransaction([]hivego.HiveOperation{rotationTx})
	err = ms.hiveCreator.PopulateSigningProps(&tx, []int{int(bh)})

	if err != nil {
		fmt.Println("Error populating signing props", err)
		return signingPackage{}, err
	}

	txId, _ := tx.GenerateTrxId()

	return signingPackage{
		Ops:  []hivego.HiveOperation{rotationTx},
		Tx:   tx,
		TxId: txId,
	}, nil
}

// buildActionBatch deterministically builds the signed (unsigned-yet) action
// batch tx for block height bh, with NO DB mutations. Used by both the leader
// (via executeActions) and cosigners (via p2p HandleMessage). The pure form
// is the load-bearing half of the cosigner split-brain fix — cosigners must
// not mark actions "processing" in their own DB, because if the leader's
// broadcast fails the cosigner is left stuck with actions that never
// complete and never auto-requeue.
func (ms *MultiSig) buildActionBatch(bh uint64) (signingPackage, []string, error) {
	if bh%ACTION_INTERVAL != 0 {
		return signingPackage{}, nil, errors.New("invalid slot")
	}
	actionFilter := []string{
		"withdraw", "stake", "unstake",
	}
	actions, err := ms.ledgerActions.GetPendingActions(bh, actionFilter...)

	if err != nil {
		return signingPackage{}, nil, err
	}
	if len(actions) == 0 {
		return signingPackage{}, nil, errors.New("no actions to process")
	}

	ops := []hivego.HiveOperation{}
	stakeBal := uint64(0)
	unstakeBal := uint64(0)
	stakeTxCount := 0
	unstakeTxCount := 0
	executedOps := make([]string, 0)
	for _, action := range actions {
		executedOps = append(executedOps, action.Id)
		if action.Type == "withdraw" {
			splitTo := strings.Split(action.To, ":")
			net := splitTo[0]
			to := splitTo[1]

			if net != "hive" {
				continue
			}
			amt := action.Amount

			op := ms.hiveCreator.Transfer(
				ms.sconf.GatewayWallet(),
				to,
				hive.AmountToString(amt),
				ms.toHiveAssetName(action.Asset),
				action.Memo,
			)

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
		mustStakeBal := int64(stakeBal - unstakeBal)

		amtStr := hive.AmountToString(mustStakeBal)

		op := ms.hiveCreator.TransferToSavings(
			ms.sconf.GatewayWallet(),
			ms.sconf.GatewayWallet(),
			amtStr,
			ms.toHiveAssetName("hbd"),
			"Staking "+amtStr+" HBD from "+strconv.Itoa(stakeTxCount)+" transactions",
		)

		ops = append(ops, op)
	} else if unstakeBal > stakeBal {
		mustUnstakeBal := int64(unstakeBal - stakeBal)

		amtStr := hive.AmountToString(mustUnstakeBal)

		op := ms.hiveCreator.TransferFromSavings(ms.sconf.GatewayWallet(), ms.sconf.GatewayWallet(), amtStr, ms.toHiveAssetName("hbd"), "Unstaking "+amtStr+" HBD from "+strconv.Itoa(unstakeTxCount)+" transactions", int(bh))

		ops = append(ops, op)
	}

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

	headerOp := ms.hiveCreator.CustomJson([]string{ms.sconf.GatewayWallet()}, []string{}, "vsc.actions", headerStr)

	ops = append([]hivego.HiveOperation{headerOp}, ops...)
	tx := ms.hiveCreator.MakeTransaction(ops)

	ms.hiveCreator.PopulateSigningProps(&tx, []int{int(bh)})

	txId, _ := tx.GenerateTrxId()

	return signingPackage{
		Ops:         ops,
		Tx:          tx,
		TxId:        txId,
		ExecutedOps: executedOps,
	}, executedOps, nil
}

// executeActions is the leader-only entry: build the batch, then mark
// every selected action "processing" in the local DB so the next
// ACTION_INTERVAL tick does not re-select and re-pay the same withdrawals
// before this batch's L1 `vsc.actions` header is re-ingested
// (IndexActions -> ExecuteComplete). Fail-safe: a broadcast failure
// triggers RevertToPending in TickActions so the actions are
// retriable on the next tick rather than stuck forever.
func (ms *MultiSig) executeActions(bh uint64) (signingPackage, error) {
	pkg, executedOps, err := ms.buildActionBatch(bh)
	if err != nil {
		return signingPackage{}, err
	}

	ms.ledgerActions.SetProcessing(executedOps...)

	return pkg, nil
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

	if balRecord != nil {
		if balRecord.BlockHeight > bh-SYNC_INTERVAL {
			return signingPackage{}, errors.New("no sync to process")
		}
	}

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

			// fmt.Println("syncBalance - appending", record.Account, record.HBD)
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
		op := ms.hiveCreator.TransferToSavings(
			ms.sconf.GatewayWallet(),
			ms.sconf.GatewayWallet(),
			hive.AmountToString(hbdToStake),
			ms.toHiveAssetName("hbd"),
			"Staking "+hive.AmountToString(hbdToStake)+" HBD",
		)

		ops = append(ops, op)
	} else if (hbdToUnstake > 10_000 || stakedBal < 10_000) && hbdToUnstake != 0 {
		op := ms.hiveCreator.TransferFromSavings(ms.sconf.GatewayWallet(), ms.sconf.GatewayWallet(), hive.AmountToString(hbdToUnstake), ms.toHiveAssetName("hbd"), "Unstaking "+hive.AmountToString(hbdToUnstake)+" HBD", int(bh+1))

		ops = append(ops, op)
	}

	if len(ops) > 0 {
		header := map[string]interface{}{
			"stake_amt":   hbdToStake,
			"unstake_amt": hbdToUnstake,
		}

		headerBytes, _ := json.Marshal(header)

		headerOp := ms.hiveCreator.CustomJson(
			[]string{ms.sconf.GatewayWallet()},
			[]string{},
			"vsc.fr_sync",
			string(headerBytes),
		)

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
	accountData, err := ms.accountClient.GetAccount([]string{ms.sconf.GatewayWallet()})

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

func (ms *MultiSig) waitForSigs(
	ctx context.Context,
	tx hivego.HiveTransaction,
	hivetxId string,
) ([]string, uint64, error) {
	threshold, publicList, weights, thErr := ms.getThreshold()
	if thErr != nil || threshold <= 0 {
		// review2 HIGH #80: with threshold==0 the collection loop
		// `for threshold > signedWeight` exits immediately and returns 0
		// signatures, which the callers then treat as "fully signed".
		return nil, 0, fmt.Errorf("getThreshold failed: %w (threshold=%d)", thErr, threshold)
	}
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
	txHash := hivego.HashTxForSig(txBytes, ms.sconf.HiveChainId())

	// var timeoutz time.Duration
	// if len(timeout) > 0 {
	// 	timeoutz = timeout[0]
	// } else {
	// 	timeoutz = 20 * time.Second
	// }

	// go func() {
	// 	time.Sleep(timeoutz)
	// 	fmt.Println("waitForSigs Timeout waiting for signatures")
	// 	if ms.msgChan[txId] != nil {
	// 		ms.msgChan[txId] <- nil
	// 	}
	// }()

	end := make(chan struct{})

	signedWeight := uint64(0)
	sigs := make([]string, 0)
	go func() {
		for uint64(threshold) > signedWeight {
			msg := <-ms.msgChan[txId]

			if msg.Type == "sign_response" {
				sigRes := signResponse{}
				err := json.Unmarshal([]byte(msg.Data), &sigRes)

				if err == nil {

					pubKey, err := RecoverPublicKey(sigRes.Sig, txHash)
					if err != nil {
						continue
						// return nil, 0, err
					}
					idx := slices.Index(publicList, pubKey)
					if idx != -1 {
						if !slices.Contains(sigs, sigRes.Sig) {
							sigs = append(sigs, sigRes.Sig)
							signedWeight = signedWeight + uint64(weights[idx])
						}
					}
				}
			}
		}

		end <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		fmt.Println("[ms] collect sigs timeout")
		return sigs, signedWeight, nil
	case <-end:
		fmt.Println("[ms] collected needed sigs")
		return sigs, signedWeight, nil
	}
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
		// review2 LOW #113: ms.bh-10 underflows to ~1.8e19 when the node
		// is below block 10, rejecting every early block as "too far into
		// past". blockHeight+10 < ms.bh is the same check without underflow.
	} else if blockHeight+10 < ms.bh {
		return errors.New("too far into past")
	}

	return nil
}

func (ms *MultiSig) getSigningKp() *hivego.KeyPair {
	kp, err := GatewayKeyFromBlsSeed(ms.identity.Get().BlsPrivKeySeed)
	if err != nil {
		fmt.Println("Failed to decode bls priv seed", err)
		return nil
	}
	return kp
}

func (ms *MultiSig) toHiveAssetName(asset string) string {
	// TODO: transition to NAI format instead of strings
	if ms.sconf.OnTestnet() {
		switch asset {
		case "hive":
			return "TESTS"
		case "hbd":
			return "TBD"
		default:
			return ""
		}
	} else {
		return strings.ToUpper(asset)
	}
}

var _ a.Plugin = &MultiSig{}

func New(
	sconf systemconfig.SystemConfig,
	witnessDb witnesses.Witnesses,
	electionDb elections.Elections,
	ledgerActions ledgerDb.BridgeActions,
	balanceDb ledgerDb.Balances,
	hiveCreator hive.HiveTransactionCreator,
	hiveConsumer *blockconsumer.HiveConsumer,
	p2p *libp2p.P2PServer,
	se *stateEngine.StateEngine,
	identityConfig common.IdentityConfig,
	hiveClient *hivego.HiveRpcNode,
) *MultiSig {
	return &MultiSig{
		witnessDb:     witnessDb,
		electionDb:    electionDb,
		ledgerActions: ledgerActions,
		balanceDb:     balanceDb,
		hiveCreator:   hiveCreator,
		hiveConsumer:  hiveConsumer,
		p2p:           p2p,
		se:            se,
		identity:      identityConfig,
		sconf:         sconf,
		hiveClient:    hiveClient,
		accountClient: hiveClient,

		msgChan: make(map[string]chan *p2pMessage),
	}
}
