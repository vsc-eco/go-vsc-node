package gateway

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// msgMu guards every access to msgChan. The map is read by the pubsub
	// dispatch goroutine (p2p.go HandleMessage) while the Tick* goroutines
	// (TickKeyRotation/TickActions/TickSyncFr) concurrently create and delete
	// entries. Without this lock the Go runtime aborts the process with
	// "fatal error: concurrent map writes".
	msgMu sync.Mutex

	bh uint64

	electionPeerIDs   atomic.Pointer[map[string]bool]
	lastElectionEpoch uint64
}

func (ms *MultiSig) Init() error {
	ms.hiveConsumer.RegisterBlockTick("multisig.tick", ms.BlockTick, false)

	// One-time rollout heal: the legacy "processing" intermediate state was
	// removed (it caused the cosigner split-brain — a cosigner that marked a
	// batch "processing" was stranded forever if the leader's broadcast failed).
	// Re-queue any actions left "processing" by the old code so they settle.
	// Safe: a landed payout is "complete" (atomic vsc.actions header), so a
	// "processing" action never settled and cannot be double-paid. Idempotent —
	// nothing writes "processing" anymore, so this is a no-op after rollout.
	if reverted, err := ms.ledgerActions.RevertProcessingToPending(); err != nil {
		fmt.Println("Multisig: failed to revert stranded 'processing' actions:", err)
	} else if len(reverted) > 0 {
		fmt.Printf("Multisig: rollout heal — re-queued %d stranded 'processing' action(s) to 'pending'\n", len(reverted))
		for _, a := range reverted {
			fmt.Printf("  requeued action id=%s to=%s amount=%d %s type=%s block=%d\n",
				a.Id, a.To, a.Amount, a.Asset, a.Type, a.BlockHeight)
		}
	}
	return nil
}

// setMsgChan creates a buffered collection channel for txId, registers it, and
// returns it. All msgChan mutations go through these helpers so the lock
// discipline lives in one place.
func (ms *MultiSig) setMsgChan(txId string) chan *p2pMessage {
	ch := make(chan *p2pMessage, 16)
	ms.msgMu.Lock()
	ms.msgChan[txId] = ch
	ms.msgMu.Unlock()
	return ch
}

// getMsgChan returns the channel for txId, or nil if none is registered.
func (ms *MultiSig) getMsgChan(txId string) chan *p2pMessage {
	ms.msgMu.Lock()
	defer ms.msgMu.Unlock()
	return ms.msgChan[txId]
}

// deleteMsgChan unregisters the channel for txId.
func (ms *MultiSig) deleteMsgChan(txId string) {
	ms.msgMu.Lock()
	delete(ms.msgChan, txId)
	ms.msgMu.Unlock()
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
var ACTION_INTERVAL = uint64(20)        // One minute of Hive blocks
var SYNC_INTERVAL = uint64(7200)        // Every 6 hours

// Devnet-only env-var overrides so e2e tests can drive rotation/action
// ticks on the order of seconds instead of an hour. Honoured only when
// set to a positive value; production binaries leave the defaults alone.
// Documented under tests/devnet/README.md.
func init() {
	if v := os.Getenv("VSC_GATEWAY_ROTATION_INTERVAL"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			ROTATION_INTERVAL = n
		}
	}
	if v := os.Getenv("VSC_GATEWAY_ACTION_INTERVAL"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			ACTION_INTERVAL = n
		}
	}
	if v := os.Getenv("VSC_GATEWAY_SYNC_INTERVAL"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			SYNC_INTERVAL = n
		}
	}
}

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

	if ms.electionPeerIDs.Load() == nil || bh%ACTION_INTERVAL == 0 {
		election, err := ms.electionDb.GetElectionByHeight(bh)
		if err == nil && election.Epoch != ms.lastElectionEpoch {
			peerIDs := make(map[string]bool, len(election.Members))
			for _, member := range election.Members {
				w, _ := ms.witnessDb.GetWitnessAtHeight(member.Account, &bh)
				if w != nil && w.PeerId != "" {
					peerIDs[w.PeerId] = true
				}
			}
			ms.electionPeerIDs.Store(&peerIDs)
			ms.lastElectionEpoch = election.Epoch
		}
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

	ms.setMsgChan(signPkg.TxId)
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
	ms.deleteMsgChan(signPkg.TxId)

	if err != nil {
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weightMeetsThreshold(weight, threshold) {
		rotationId, err := ms.hiveCreator.Broadcast(tx)

		fmt.Println("Rotation txId", rotationId, err)
	}
}

func (ms *MultiSig) TickActions(bh uint64) {
	signPkg, err := ms.executeActions(bh)

	fmt.Println("TickActions", err, signPkg)
	if err != nil {
		return
	}

	ms.setMsgChan(signPkg.TxId)
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
		// an under-signed actions transaction.
		fmt.Println("TickActions getThreshold failed, aborting", thErr, threshold)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	signatures, weight, err := ms.waitForSigs(ctx, signPkg.Tx, signPkg.TxId)
	ms.deleteMsgChan(signPkg.TxId)

	fmt.Println("TickActions signatures", signatures, weight, err)
	if err != nil {
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weightMeetsThreshold(weight, threshold) {
		rotationId, err := ms.hiveCreator.Broadcast(tx)

		fmt.Println("Actions txId", rotationId, err)
	}
}

func (ms *MultiSig) TickSyncFr(bh uint64) {

	signPkg, err := ms.syncBalance(bh)

	fmt.Println("signPkg, err", signPkg, err)
	if err != nil {
		return
	}

	ms.setMsgChan(signPkg.TxId)
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
	ms.deleteMsgChan(signPkg.TxId)

	if err != nil {
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weightMeetsThreshold(weight, threshold) {
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

	// Collect (account, gatewayKey, stake) for every eligible elected member.
	// stake is the on-chain election weight; it drives the proportional
	// multisig share assigned below. All three inputs are on-chain/deterministic.
	type gwMember struct {
		account string
		key     string
		stake   uint64
	}
	eligible := make([]gwMember, 0, len(electionResult.Members))
	for idx, member := range electionResult.Members {
		witnessData, _ := ms.witnessDb.GetWitnessAtHeight(member.Account, &bh)
		if witnessData == nil {
			fmt.Println("No witness data for", member.Account, "at", bh)
			continue
		}
		if witnessData.GatewayKey == "" {
			continue
		}
		// FUZZ-1: skip witnesses whose announced gateway_key would panic the
		// hivego serializer (DecodePublicKey slices decoded[-4:] on short
		// inputs). Without this guard a single malicious witness can crash
		// every elected witness on the next rotation tick.
		if err := safeValidateGatewayKey(witnessData.GatewayKey); err != nil {
			log.Warn("skipping witness with malformed gateway_key",
				"account", member.Account, "bh", bh, "err", err)
			continue
		}
		eligible = append(eligible, gwMember{
			account: member.Account,
			key:     witnessData.GatewayKey,
			stake:   electionResult.Weights[idx],
		})
	}

	// Keep the highest-stake signers when more than MAX_GATEWAY_KEYS witnesses
	// are eligible. Sort by stake DESC, tiebreaking on the globally-unique
	// account name so every node selects the identical set in the identical
	// order (determinism). review2 MEDIUM #57: compare the uint64 stakes
	// directly — int(uint64) truncates stakes >= 2^63 and int-subtraction
	// overflows, which would rank the largest stakers as the smallest.
	slices.SortFunc(eligible, func(a, b gwMember) int {
		return cmp.Or(
			cmp.Compare(b.stake, a.stake),
			cmp.Compare(a.account, b.account),
		)
	})
	if len(eligible) > MAX_GATEWAY_KEYS {
		eligible = eligible[:MAX_GATEWAY_KEYS]
	}
	if len(eligible) < MIN_GATEWAY_KEYS {
		return signingPackage{}, errors.New("not enough keys")
	}

	// Assign each signer a uint16 multisig weight proportional to its stake.
	stakes := make([]uint64, len(eligible))
	accounts := make([]string, len(eligible))
	for i, m := range eligible {
		stakes[i] = m.stake
		accounts[i] = m.account
	}
	assignedWeights := quantizeStakeWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE)

	gatewayKeys := make([][2]interface{}, len(eligible))
	totalWeight := 0
	for i, m := range eligible {
		gatewayKeys[i] = [2]interface{}{m.key, assignedWeights[i]}
		totalWeight += assignedWeights[i]
	}

	var e [2]interface{}
	e[0] = "vsc.dao"
	e[1] = 1

	var eb [2]interface{}
	eb[0] = "vsc.network"
	eb[1] = 1

	// review2 HIGH #29: ceil(2N/3), not floor — floor let a sub-2/3 set of
	// gateway signers move funds. totalWeight is the SUM of the assigned
	// stake-proportional weights, not the key count.
	weightThreshold := gatewayWeightThreshold(totalWeight)

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
		KeyAuths:     gatewayKeys,
		AccountAuths: [][2]any{},
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

			amtStr, err := common.FormatAssetAmount(amt, action.Asset)
			if err != nil {
				log.Warn(
					"skipping withdraw action: cannot format amount",
					"action",
					action.Id,
					"asset",
					action.Asset,
					"err",
					err,
				)
				continue
			}
			op := ms.hiveCreator.Transfer(
				ms.sconf.GatewayWallet(),
				to,
				amtStr,
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
		//Must stake
		mustStakeBal := int64(stakeBal - unstakeBal)

		amtStr, err := common.FormatAssetAmount(mustStakeBal, "hbd")
		if err != nil {
			return signingPackage{}, fmt.Errorf("format stake amount: %w", err)
		}

		op := ms.hiveCreator.TransferToSavings(
			ms.sconf.GatewayWallet(),
			ms.sconf.GatewayWallet(),
			amtStr,
			ms.toHiveAssetName("hbd"),
			"Staking "+amtStr+" HBD from "+strconv.Itoa(stakeTxCount)+" transactions",
		)

		ops = append(ops, op)
	} else if unstakeBal > stakeBal {
		//Must unstake
		mustUnstakeBal := int64(unstakeBal - stakeBal)

		amtStr, err := common.FormatAssetAmount(mustUnstakeBal, "hbd")
		if err != nil {
			return signingPackage{}, fmt.Errorf("format unstake amount: %w", err)
		}

		op := ms.hiveCreator.TransferFromSavings(ms.sconf.GatewayWallet(), ms.sconf.GatewayWallet(), amtStr, ms.toHiveAssetName("hbd"), "Unstaking "+amtStr+" HBD from "+strconv.Itoa(unstakeTxCount)+" transactions", int(bh))

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

	headerOp := ms.hiveCreator.CustomJson([]string{ms.sconf.GatewayWallet()}, []string{}, "vsc.actions", headerStr)

	ops = append([]hivego.HiveOperation{headerOp}, ops...)
	tx := ms.hiveCreator.MakeTransaction(ops)

	ms.hiveCreator.PopulateSigningProps(&tx, []int{int(bh)})

	txId, _ := tx.GenerateTrxId()

	// executeActions is intentionally PURE — it builds the deterministic batch
	// but does not mutate action state. It is called by both the leader
	// (TickActions) and every cosigner (p2p HandleMessage on a sign_request),
	// so a per-node status mutation here was the cosigner split-brain bug: a
	// cosigner marked the batch "processing" in its own DB and, if the leader's
	// broadcast then failed, was left with actions stuck "processing" forever
	// (never re-selected, never completed). Re-selection safety instead comes
	// from L1 settlement (status -> "complete" on the re-ingested vsc.actions
	// header) plus the ACTION_INTERVAL (20 blocks) > tx-expiry (~10 blocks)
	// timing — a re-selected action's prior attempt is, by the next tick, either
	// already settled (and excluded) or permanently expired.

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
	balRecords, err := ms.balanceDb.GetAll(bh)
	if err != nil {
		return signingPackage{}, errors.New("balance enumeration failed")
	}
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
		stakeAmtStr, err := common.FormatAssetAmount(hbdToStake, "hbd")
		if err != nil {
			return signingPackage{}, fmt.Errorf("format stake amount: %w", err)
		}
		op := ms.hiveCreator.TransferToSavings(
			ms.sconf.GatewayWallet(),
			ms.sconf.GatewayWallet(),
			stakeAmtStr,
			ms.toHiveAssetName("hbd"),
			"Staking "+stakeAmtStr+" HBD",
		)

		ops = append(ops, op)
	} else if (hbdToUnstake > 10_000 || stakedBal < 10_000) && hbdToUnstake != 0 {
		unstakeAmtStr, err := common.FormatAssetAmount(hbdToUnstake, "hbd")
		if err != nil {
			return signingPackage{}, fmt.Errorf("format unstake amount: %w", err)
		}
		op := ms.hiveCreator.TransferFromSavings(ms.sconf.GatewayWallet(), ms.sconf.GatewayWallet(), unstakeAmtStr, ms.toHiveAssetName("hbd"), "Unstaking "+unstakeAmtStr+" HBD", int(bh+1))

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
	// Capture the channel once, locally. The collector goroutine below reads
	// from this local — never from msgChan — so it cannot block forever on a
	// nil read after the caller deletes the map entry on return (the leak this
	// guards against).
	ch := ms.getMsgChan(txId)
	if ch == nil {
		return nil, 0, errors.New("no channel for txId")
	}

	txBytes, err := hivego.SerializeTx(tx)

	if err != nil {
		return nil, 0, err
	}
	txHash := hivego.HashTxForSig(txBytes, ms.sconf.HiveChainId())

	type collectResult struct {
		sigs   []string
		weight uint64
	}
	// Buffered so the collector can always hand back its result and exit, even
	// if the parent already returned via ctx timeout — no goroutine leak.
	resCh := make(chan collectResult, 1)

	go func() {
		signedWeight := uint64(0)
		sigs := make([]string, 0)
		// S1: dedup on the recovered signer pubkey, not on the raw signature
		// string. ECDSA sigs are malleable — a single signer can submit (r,s)
		// and (r,N-s,v^1) which differ as strings but recover the same pubkey;
		// without this map the signer's weight is counted twice. RecoverPublicKey
		// additionally rejects high-S sigs so the malleated form never reaches
		// here, but we still key dedup on pubkey for defense in depth.
		signedByPubKey := make(map[string]struct{})
		for uint64(threshold) > signedWeight {
			select {
			case <-ctx.Done():
				// Timed out / cancelled: hand back what we have and exit.
				resCh <- collectResult{sigs, signedWeight}
				return
			case msg := <-ch:
				if msg == nil {
					continue
				}
				if msg.Type == "sign_response" {
					sigRes := signResponse{}
					err := json.Unmarshal([]byte(msg.Data), &sigRes)

					if err == nil {

						pubKey, err := RecoverPublicKey(sigRes.Sig, txHash)
						if err != nil {
							continue
						}
						idx := slices.Index(publicList, pubKey)
						if idx != -1 {
							if _, already := signedByPubKey[pubKey]; !already {
								signedByPubKey[pubKey] = struct{}{}
								sigs = append(sigs, sigRes.Sig)
								signedWeight = signedWeight + uint64(weights[idx])
							}
						}
					}
				}
			}
		}

		resCh <- collectResult{sigs, signedWeight}
	}()

	// sigs/signedWeight are owned exclusively by the collector goroutine and
	// only ever read here after it sends them on resCh — so there is no
	// concurrent read/write of the slice or counter.
	select {
	case <-ctx.Done():
		res := <-resCh
		fmt.Println("[ms] collect sigs timeout")
		return res.sigs, res.weight, nil
	case res := <-resCh:
		fmt.Println("[ms] collected needed sigs")
		return res.sigs, res.weight, nil
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
	if ms.sconf.OnTestnet() || ms.sconf.OnDevnet() {
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
