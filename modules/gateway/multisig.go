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
	"vsc-node/modules/common/consensusversion"
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

	// solvency is the v0.6.0 vault-solvency monitor (brief fix 1); nil unless the
	// operator opts in via VSC_SOLVENCY_MONITOR. Node-local, off the consensus path.
	solvency *SolvencyMonitor
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
		log.Error("Multisig: failed to revert stranded 'processing' actions", "err", err)
	} else if len(reverted) > 0 {
		log.Info("Multisig: rollout heal — re-queued stranded 'processing' actions", "count", len(reverted))
		for _, a := range reverted {
			log.Info("requeued action", "id", a.Id, "to", a.To, "amount", a.Amount, "asset", a.Asset, "type", a.Type, "block", a.BlockHeight)
		}
	}

	// v0.6.0 vault protections (brief fix 1): construct the solvency monitor,
	// fully wired — the LiabilitySource sums expected liability from the L2 ledger
	// balance snapshot (ms.balanceDb), and the HaltTrigger broadcasts a node-signed
	// vsc.halt (ms.broadcastHalt). It stays OFF unless the operator sets
	// VSC_SOLVENCY_MONITOR, and even when enabled it alarms before it halts and
	// degrades on missing data — so it cannot spuriously halt.
	ms.solvency = NewSolvencyMonitorFromEnv(
		ms.sconf.GatewayWallet(),
		ms.accountClient,
		newLedgerLiabilitySource(ms.balanceDb),
		ms.broadcastHalt,
	)
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

// Canonical mainnet intervals. The VSC_GATEWAY_*_INTERVAL env overrides applied
// in init() are devnet-only; on mainnet these are enforced regardless.
const (
	mainnetRotationInterval = uint64(20 * 60)
	mainnetActionInterval   = uint64(20)
	mainnetSyncInterval     = uint64(7200)
)

// enforceMainnetIntervals forces the canonical rotation/action/sync intervals on
// mainnet, overriding any VSC_GATEWAY_*_INTERVAL value that init() picked up
// from the environment. review7 C11-c: these intervals gate deterministic ticks
// (bh % INTERVAL), so a mainnet node running a divergent interval — set
// accidentally or maliciously by anyone with env access — would fork. The
// overrides remain available off-mainnet for e2e tests.
func enforceMainnetIntervals(onMainnet bool) {
	if !onMainnet {
		return
	}
	if ROTATION_INTERVAL != mainnetRotationInterval ||
		ACTION_INTERVAL != mainnetActionInterval ||
		SYNC_INTERVAL != mainnetSyncInterval {
		log.Warn("ignoring VSC_GATEWAY_*_INTERVAL override on mainnet — forcing canonical intervals",
			"rotationWas", ROTATION_INTERVAL, "actionWas", ACTION_INTERVAL, "syncWas", SYNC_INTERVAL)
		ROTATION_INTERVAL = mainnetRotationInterval
		ACTION_INTERVAL = mainnetActionInterval
		SYNC_INTERVAL = mainnetSyncInterval
	}
}

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

	// v0.6.0 fix 1: solvency sampling runs on EVERY node (not just the slot
	// leader, so it sits above the leader gate below) — any honest node that sees
	// a depleted real balance can independently trip. Off the consensus path.
	if ms.solvency != nil && bh%ms.solvency.interval == 0 {
		go ms.solvency.Check(bh)
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
			log.Debug("running key rotation", "bh", bh)
			go ms.TickKeyRotation(bh)
		}
		if bh%ACTION_INTERVAL == 0 {
			log.Debug("running actions", "bh", bh)
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
		// H-5: surface rotation failures (e.g. "not enough keys" when the
		// committee is below the gateway floor of 8) instead of the previous
		// silent bare return that hid an indefinitely-wedged gateway rotation
		// with no log, metric, or chain event.
		log.Warn("gateway key rotation skipped", "bh", bh, "err", err)
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
		log.Error("keyRotation getThreshold failed, aborting", "err", thErr, "threshold", threshold)
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

		log.Verbose("rotation broadcast", "txId", rotationId, "err", err)
	}
}

func (ms *MultiSig) TickActions(bh uint64) {
	signPkg, err := ms.executeActions(bh)

	if err != nil {
		log.Verbose("TickActions: executeActions error", "err", err)
		return
	}
	log.Verbose("TickActions: collected actions", "txId", signPkg.TxId)

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

	threshold, _, _, thErr := ms.getThreshold()
	if thErr != nil || threshold <= 0 {
		// review2 HIGH #80: see keyRotation — abort instead of broadcasting
		// an under-signed actions transaction.
		log.Error("TickActions getThreshold failed, aborting", "err", thErr, "threshold", threshold)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	signatures, weight, err := ms.waitForSigs(ctx, signPkg.Tx, signPkg.TxId)
	ms.deleteMsgChan(signPkg.TxId)

	log.Verbose("TickActions: collected signatures", "count", len(signatures), "weight", weight, "err", err)
	if err != nil {
		return
	}

	tx := signPkg.Tx
	for _, sig := range signatures {
		tx.AddSig(sig)
	}

	if weightMeetsThreshold(weight, threshold) {
		rotationId, err := ms.hiveCreator.Broadcast(tx)

		log.Debug("actions broadcast", "txId", rotationId, "err", err)
	}
}

func (ms *MultiSig) TickSyncFr(bh uint64) {

	signPkg, err := ms.syncBalance(bh)

	if err != nil {
		log.Verbose("TickSyncFr: syncBalance error", "err", err)
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
		log.Error("TickSyncFr getThreshold failed, aborting", "err", thErr, "threshold", threshold)
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

		log.Debug("sync_fr broadcast", "txId", rotationId, "err", err)
	}
}

func (ms *MultiSig) keyRotation(bh uint64) (signingPackage, error) {
	if bh%ACTION_INTERVAL != 0 {
		return signingPackage{}, errors.New("invalid slot")
	}
	// v0.6.0 vault protections (brief fix 2, committee-churn freeze): while an
	// outbound halt is active, do NOT rotate the gateway multisig authority.
	// Rotation rebuilds the signing set from the current committee; performing it
	// mid-halt would let an attacker churn honest cosigners out (or a colluding
	// subset in) exactly during the response window. Deferring rotation keeps the
	// existing authority intact until the halt expires or is lifted — the freeze
	// is a safe no-op that simply postpones the next rotation.
	if ms.outboundHalted(bh) {
		return signingPackage{}, errOutboundHalted
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
			log.Verbose("no witness data", "account", member.Account, "bh", bh)
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
	// Dedup by gateway pubkey before the cap + weight assignment (ported from the
	// bond-window commit). If two elected witnesses announced the SAME gateway_key
	// (no uniqueness gate at the announce path — sibling of audit H-6's BLS-key
	// collision), an account_update carrying a duplicate key_auths entry is
	// rejected by Hive (wedging the rotation) and the duplicate would double-count
	// in the stake weighting below. Keep the first occurrence in the deterministic
	// descending-stake order so every cosigner drops the identical duplicate.
	if len(eligible) > 1 {
		seen := make(map[string]struct{}, len(eligible))
		deduped := eligible[:0]
		for _, m := range eligible {
			if _, dup := seen[m.key]; dup {
				continue
			}
			seen[m.key] = struct{}{}
			deduped = append(deduped, m)
		}
		eligible = deduped
	}
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

	// CP-4 (audit A3-2): removal of the vsc.dao OWNER backstop is gated on the
	// v0.2.0 activation (GatewayDecentralizationActive) — gateway decentralization
	// ships as part of the coordinated v0.2.0 batch rather than a standalone height.
	// At/after activation the OWNER auth is the committee keys ONLY and posting
	// carries only vsc.network — only the elected committee (2/3-of-stake
	// supermajority) can re-key or move custody, with no single-account backstop.
	// BELOW activation (and on any network where v0.2.0 is unpinned) the vsc.dao
	// backstop is RETAINED exactly as base 937ae771: owner carries vsc.dao at
	// weight == threshold and posting carries vsc.dao + vsc.network.
	//
	// *** HIGHEST-RISK once active ***: with no vsc.dao backstop, a committee that
	// wedges below threshold has no on-chain recovery. Do NOT raise the consensus
	// floor to 0.2.0 on mainnet until CP-1 (floor-advance readiness, gateway/TSS
	// handover ordering, the floor guard) is devnet-proven under heavy churn.
	//
	// Determinism (Constraint 3): decentralizeOwner is a pure function of the
	// chain-active consensus version at bh (ResultVersion of the election at bh,
	// already loaded above) + the compile-time line, so every cosigner builds the
	// identical account_update.
	decentralizeOwner := consensusversion.GatewayDecentralizationActive(elections.ResultVersion(electionResult))

	var eb [2]interface{}
	eb[0] = "vsc.network"
	eb[1] = 1

	// review2 HIGH #29: ceil(2N/3), not floor — floor let a sub-2/3 set of
	// gateway signers move funds. totalWeight is the SUM of the assigned
	// stake-proportional weights, not the key count.
	weightThreshold := gatewayWeightThreshold(totalWeight)

	// Build owner + posting AccountAuths per the v0.2.0 decentralization gate
	// (audit A3-2). Active → committee keys only (vsc.dao dropped from owner;
	// posting carries only vsc.network). Inactive (default / v0.2.0 unpinned) →
	// retain the vsc.dao backstop exactly as base 937ae771: owner vsc.dao@threshold
	// (can meet the owner threshold alone) + posting vsc.dao@1 + vsc.network@1.
	var ownerAccountAuths [][2]any
	var postingAccountAuths [][2]any
	if decentralizeOwner {
		ownerAccountAuths = [][2]any{}
		postingAccountAuths = [][2]any{eb}
	} else {
		var o [2]interface{}
		o[0] = "vsc.dao"
		o[1] = weightThreshold // backstop: vsc.dao can meet the owner threshold alone
		var e [2]interface{}
		e[0] = "vsc.dao"
		e[1] = 1
		ownerAccountAuths = [][2]any{o}
		postingAccountAuths = [][2]any{e, eb}
	}

	jsonMetadata := map[string]interface{}{
		"msg":                 "Gateway wallet for the VSC Network",
		"website":             "https://vsc.network",
		"epoch":               electionResult.Epoch,
		"last_block_rotation": bh,
	}

	jsonBytes, _ := json.Marshal(jsonMetadata)

	rotationTx := ms.hiveCreator.UpdateAccount(ms.sconf.GatewayWallet(), &hivego.Auths{
		WeightThreshold: weightThreshold,
		KeyAuths:        gatewayKeys,
		AccountAuths:    ownerAccountAuths,
	}, &hivego.Auths{
		WeightThreshold: weightThreshold,
		KeyAuths:        gatewayKeys,
		AccountAuths:    [][2]any{},
	}, &hivego.Auths{
		WeightThreshold: 1,
		AccountAuths:    postingAccountAuths,
		KeyAuths:        [][2]any{},
	}, string(jsonBytes), "STM8buQNWovTcX7H8yLdYNx82xDddQE9R5MzQDNg4mocScnXTGSkE")

	tx := ms.hiveCreator.MakeTransaction([]hivego.HiveOperation{rotationTx})
	err = ms.hiveCreator.PopulateSigningProps(&tx, []int{int(bh)})

	if err != nil {
		log.Warn("Error populating signing props", "err", err)
		return signingPackage{}, err
	}

	txId, _ := tx.GenerateTrxId()

	return signingPackage{
		Ops:  []hivego.HiveOperation{rotationTx},
		Tx:   tx,
		TxId: txId,
	}, nil
}

// errOutboundHalted is returned by executeActions while an outbound halt is
// active; TickActions treats it like any other "nothing to do" and skips the
// broadcast, so no gateway action tx is assembled.
var errOutboundHalted = errors.New("outbound halted")

// outboundHalted reports whether the gateway OUTBOUND path is frozen at bh by an
// active outbound-halt entry (v0.6.0 vault protections, brief fixes 1 & 3). The
// verdict is deterministic on-chain state (height-addressable halt entries), so
// every cosigner reproduces the identical decision → identical batch → CID. It
// is inert below the 0.6.0 floor, keeping pre-activation gateway behavior
// byte-identical.
func (ms *MultiSig) outboundHalted(bh uint64) bool {
	if ms.se == nil {
		return false
	}
	if !consensusversion.OutboundHaltActive(ms.se.ActiveConsensusVersion(bh)) {
		return false
	}
	return ms.se.OutboundHaltedAt(bh)
}

// Value-scaled outbound delay tiers (v0.6.0 vault protections, brief fix 4).
// Amounts are raw ledger integer units — HBD/HIVE carry 3 decimals, so 1_000 =
// 1.000 coin. Blocks are ~3s. The curve is monotonic (bigger value → longer
// wait) and FIXED IN CODE (not a votable parameter), so governance cannot vote
// the floor to zero — sidestepping THORChain's votable-governance mistake. These
// magnitudes are a starting calibration; the intended source of truth is the
// flow-data sizing in the hot/cold design doc, and they are candidates to move to
// ConsensusParams if the network wants them tunable behind the version gate.
const (
	outboundDelayTier1Amount = int64(1_000_000)   // 1,000 coin: below this, dust-fast (no delay)
	outboundDelayTier2Amount = int64(10_000_000)  // 10,000 coin
	outboundDelayTier3Amount = int64(100_000_000) // 100,000 coin

	outboundDelayTier1Blocks = uint64(200)  // ~10 min  (1,000–10,000)
	outboundDelayTier2Blocks = uint64(1200) // ~1 h     (10,000–100,000)
	outboundDelayTier3Blocks = uint64(3600) // ~3 h     (>= 100,000, vault-sized)
)

// outboundDelayBlocks returns the minimum age (in L1 blocks) a withdrawal of the
// given amount must reach before it is eligible for a gateway batch. Dust is
// fast; vault-sized spends wait hours — the window in which the minority halt and
// off-chain recovery act. Deterministic pure function → identical on every
// cosigner.
//
// KNOWN LIMITATIONS (see VAULT-PROTECTIONS-HANDOVER.md), both intentional given
// the brief scopes fix 4 as a per-outbound speed bump backed by the aggregate
// solvency halt (fix 1):
//   - Per-TOKEN-AMOUNT, not per-value: HBD and HIVE share thresholds, so a
//     value-equal HIVE drain can land in a lower tier than the HBD equivalent.
//   - Splittable: a large drain chunked below tier 1 gets no delay; the
//     aggregate drain is what the solvency halt is meant to catch.
func outboundDelayBlocks(amount int64) uint64 {
	switch {
	case amount < outboundDelayTier1Amount:
		return 0
	case amount < outboundDelayTier2Amount:
		return outboundDelayTier1Blocks
	case amount < outboundDelayTier3Amount:
		return outboundDelayTier2Blocks
	default:
		return outboundDelayTier3Blocks
	}
}

// outboundDelayActive reports whether the value-scaled outbound delay is in force
// at bh (inert below the 0.6.0 floor, keeping pre-activation selection identical).
func (ms *MultiSig) outboundDelayActive(bh uint64) bool {
	if ms.se == nil {
		return false
	}
	return consensusversion.OutboundDelayActive(ms.se.ActiveConsensusVersion(bh))
}

// isCommitteeMember reports whether `account` is in the committee active at bh.
// Used as a pre-flight before broadcasting a solvency vsc.halt so a non-member
// node doesn't post an op executeHalt will silently ignore. Direct account match
// (both are plain Hive accounts); the on-chain executeHalt does the authoritative
// normalized membership check.
func (ms *MultiSig) isCommitteeMember(account string, bh uint64) bool {
	if ms.electionDb == nil {
		return false
	}
	elec, err := ms.electionDb.GetElectionByHeight(bh)
	if err != nil {
		return false
	}
	for _, mbr := range elec.Members {
		if mbr.Account == account {
			return true
		}
	}
	return false
}

// broadcastHalt is the solvency monitor's HaltTrigger (brief fix 1): it
// broadcasts a vsc.halt op signed by THIS node's OWN active key (required_auths =
// the node's Hive account). Because the node is a committee member, executeHalt
// accepts it as a single-node outbound halt — so a node-local, non-deterministic
// solvency detection produces a deterministic on-chain effect every node then
// processes identically. Signs+broadcasts via the same node-active-keyed
// hiveCreator the announcements module uses; the gateway multisig wallet is not
// involved. Duration is omitted so executeHalt applies the default window.
func (ms *MultiSig) broadcastHalt(bh uint64, reason string) {
	account := ms.identity.Get().HiveUsername
	if account == "" {
		log.Error("solvency: cannot broadcast vsc.halt — node identity has no Hive username", "bh", bh)
		return
	}
	// Don't broadcast below the 0.6.0 floor: executeHalt ignores vsc.halt there, so
	// it would only waste RC on a permanently-ineffective op.
	if ms.se == nil || !consensusversion.OutboundHaltActive(ms.se.ActiveConsensusVersion(bh)) {
		log.Warn("solvency: NOT broadcasting vsc.halt — outbound-halt feature inactive at this height", "bh", bh)
		return
	}
	// executeHalt only honors a setter that is a CURRENT committee member. If this
	// node's account isn't in the active committee the op is a silent on-chain
	// no-op, so skip it and surface the misconfiguration rather than log a false
	// 'halt broadcast' success.
	if !ms.isCommitteeMember(account, bh) {
		log.Error("solvency: node account is NOT in the active committee — a solvency vsc.halt "+
			"would be ignored on-chain; NOT broadcasting (check identity vs elected witness account)",
			"account", account, "bh", bh)
		return
	}
	// Body matches vsc.halt's parsed fields (state_engine.TxHalt JSON tags);
	// Duration is omitted so executeHalt applies the default window.
	body, err := json.Marshal(struct {
		Reason string `json:"reason,omitempty"`
	}{Reason: reason})
	if err != nil {
		log.Error("solvency: failed to marshal vsc.halt body", "err", err, "bh", bh)
		return
	}
	op := ms.hiveCreator.CustomJson([]string{account}, []string{}, "vsc.halt", string(body))
	tx := ms.hiveCreator.MakeTransaction([]hivego.HiveOperation{op})
	if err := ms.hiveCreator.PopulateSigningProps(&tx, nil); err != nil {
		log.Error("solvency: PopulateSigningProps for vsc.halt failed", "err", err, "bh", bh)
		return
	}
	sig, err := ms.hiveCreator.Sign(tx)
	if err != nil {
		log.Error("solvency: signing vsc.halt failed", "err", err, "bh", bh)
		return
	}
	tx.AddSig(sig)
	txId, err := ms.hiveCreator.Broadcast(tx)
	if err != nil {
		log.Error("solvency: broadcasting vsc.halt failed", "err", err, "bh", bh)
		return
	}
	log.Error("SOLVENCY HALT: vsc.halt broadcast from node account",
		"account", account, "reason", reason, "txId", txId, "bh", bh)
}

// selectEligibleActions applies the v0.6.0 value-scaled outbound delay (when
// active) and the MaxGatewayActionBatch cap TOGETHER, returning the oldest
// actions eligible to settle this tick. A withdrawal is eligible only once it has
// aged >= the delay its value warrants; stake/unstake savings-netting is never
// delayed. The cap is applied to the ELIGIBLE set, not the raw fetch, so a run of
// under-aged withdrawals at the front of the queue cannot occupy the batch window
// and starve newer eligible actions. Pure + deterministic: every cosigner
// reproduces the identical selection from the same sorted input.
func selectEligibleActions(actions []ledgerDb.ActionRecord, delayActive bool, bh uint64) []ledgerDb.ActionRecord {
	selected := make([]ledgerDb.ActionRecord, 0, ledgerDb.MaxGatewayActionBatch)
	for _, action := range actions {
		if delayActive && action.Type == "withdraw" &&
			bh < action.BlockHeight+outboundDelayBlocks(action.Amount) {
			continue // under-aged: leave pending, re-select on a later tick
		}
		selected = append(selected, action)
		if len(selected) >= ledgerDb.MaxGatewayActionBatch {
			break
		}
	}
	return selected
}

func (ms *MultiSig) executeActions(bh uint64) (signingPackage, error) {
	if bh%ACTION_INTERVAL != 0 {
		return signingPackage{}, errors.New("invalid slot")
	}
	// v0.6.0 vault protections (brief fixes 1 & 3): while an outbound halt is
	// active, assemble NOTHING — no withdrawal batch and no vsc.actions settlement
	// header is built, signed, or broadcast, so the outbound path is fully frozen.
	// Deterministic across cosigners; unpaid actions stay pending and resume on
	// the next tick once the halt expires or is lifted.
	if ms.outboundHalted(bh) {
		return signingPackage{}, errOutboundHalted
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
	// v0.6.0 fix 4 (value-scaled delay) + review7 C11-a (batch cap), applied
	// TOGETHER: a withdrawal is eligible only once it has aged >= the delay its
	// value warrants; under-aged withdrawals are skipped and left pending for a
	// later tick. Critically the MaxGatewayActionBatch cap is applied to the
	// ELIGIBLE set, not the raw fetch — so a run of under-aged withdrawals at the
	// FRONT of the queue cannot occupy the batch/settlement window and starve
	// newer eligible actions. The DB scans up to MaxGatewayActionScan (a bounded
	// multiple of the batch), giving the age-filter headroom past a burst of
	// under-aged withdrawals. Deterministic: every cosigner fetches the identical
	// sorted window, applies the same age test, and caps identically. (A flood
	// larger than the scan window only DEFERS newer actions until the oldest
	// under-aged cohort matures; the proper query-level fix is a persisted
	// eligible_height — see VAULT-PROTECTIONS-HANDOVER.md.)
	actions = selectEligibleActions(actions, ms.outboundDelayActive(bh), bh)
	if len(actions) == 0 {
		return signingPackage{}, errors.New("no eligible actions to process")
	}

	ops := []hivego.HiveOperation{}
	stakeBal := uint64(0)
	unstakeBal := uint64(0)
	stakeTxCount := 0
	unstakeTxCount := 0
	executedOps := make([]string, 0)
	for _, action := range actions {
		// ops = append(ops, ms.createWithdrawOps(action)...)
		// (value-scaled delay + batch cap already applied to `actions` above; only
		// stake/unstake savings-netting is gateway-internal and never delayed.)

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
			// audit GV-L7: int(a)-int(b) overflows / inverts sign for large int64
			// amounts → non-transitive order. Compare the int64 amounts directly.
			return cmp.Compare(a.Amount, b.Amount)
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

	log.Verbose("ChainAction.ClearedOps", "b64", b64Bytes)

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
		// audit GV-L6: guard the bh-SYNC_INTERVAL subtract — on a fresh chain
		// (bh < SYNC_INTERVAL) it underflows to ~MaxUint64, making the freshness
		// check always false and bypassing the "no sync to process" guard.
		if bh < SYNC_INTERVAL || balRecord.BlockHeight > bh-SYNC_INTERVAL {
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
		log.Verbose("waitForSigs: collect timeout", "weight", res.weight)
		return res.sigs, res.weight, nil
	case res := <-resCh:
		log.Verbose("waitForSigs: collected needed sigs", "weight", res.weight)
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
		log.Error("failed to decode bls priv seed", "err", err)
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
	// C11-c: a mainnet node must use the canonical consensus intervals,
	// regardless of any VSC_GATEWAY_*_INTERVAL env value.
	enforceMainnetIntervals(sconf.OnMainnet())
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
