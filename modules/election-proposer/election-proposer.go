package election_proposer

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/hive"
	"vsc-node/lib/utils"
	"vsc-node/lib/vsclog"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/poaseats"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	"vsc-node/modules/incentive-pendulum/rewards"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
	blsu "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

var log = vsclog.Module("ep")

type electionProposer struct {
	conf  common.IdentityConfig
	sconf systemconfig.SystemConfig

	p2p     *libp2p.P2PServer
	service libp2p.PubSubService[p2pMessage]

	witnesses witnesses.Witnesses
	elections elections.Elections
	balanceDb ledgerDb.Balances
	vscBlocks vscBlocks.VscBlocks

	// poaSeats is the POA seat registry consulted when restricting candidacy to
	// ratified seats. Nil on harnesses that do not wire POA — every read site
	// treats nil as "gate inert", which keeps pre-POA candidacy behaviour rather
	// than deleting every candidate.
	poaSeats poaseats.PoaSeats

	da *datalayer.DataLayer

	txCreator hive.HiveTransactionCreator

	hiveConsumer *blockconsumer.HiveConsumer
	se           *stateEngine.StateEngine
	// bh is the latest processed block height. Written by blockTick (under
	// electionMu) and read lock-free from the pubsub handler goroutine
	// (p2p.go + GenerateElectionAtBlock via the signer path), so it is atomic
	// to avoid a data race / word-tear (audit H-15).
	bh         atomic.Uint64
	headHeight *uint64

	sigChannels map[uint64]chan *signResponse
	signingInfo *struct {
		epoch   uint64
		block   uint64
		cid     string
		circuit *dids.PartialBlsCircuit
	}

	electionMu sync.Mutex
	sigMu      sync.Mutex
	// sigChannelsMu guards the sigChannels MAP structure only (not the channel
	// sends). Without it the map is read/written concurrently by the election
	// sig-collection path and the pubsub handler goroutine → a fatal
	// "concurrent map read and map write" node crash (audit H-15/m69a-3).
	// Look the channel up under this lock, then send OUTSIDE it (sending under
	// a lock the receiver path also needs would deadlock waitForSigs).
	sigChannelsMu sync.Mutex
	sigRunning    bool

	// scoreMapMinSamples and banThresholdPercent are the two knobs of the
	// per-witness ban rule (see computeBanScores): a witness is banned when,
	// over the blocks of the epochs it was elected to, it signed fewer than
	// banThresholdPercent% — but only once that opportunity reaches
	// scoreMapMinSamples blocks. Both are consensus-critical (they decide
	// BannedNodes → committee → election CID), so both are resolved once in
	// New() — the production defaults (500 / 75) are locked at process start and
	// cannot drift mid-run, and the env overrides are devnet-only so
	// testnet/mainnet operators cannot diverge them across nodes.
	scoreMapMinSamples  uint64
	banThresholdPercent uint64

	// Election anchors are derived deterministically (canonicalElectionAnchor)
	// from the on-chain previous-election height — no in-memory anchor map is
	// kept, so anchors survive restarts and cannot be poisoned (audit F5/CP-0b).

	// sigCollectionWindow is the BLS signature collection timeout for
	// waitForSigs. Resolved once in New() from VSC_ELECTION_SIG_COLLECTION_WINDOW
	// (devnet only) or the production default of 120s.
	sigCollectionWindow time.Duration
}

type ElectionProposer = *electionProposer

var _ a.Plugin = &electionProposer{}

// TODO: Add a way to get the current consensus version
// TODO: Add a way to get the witness active score
func New(
	p2p *libp2p.P2PServer,
	witnesses witnesses.Witnesses,
	elections elections.Elections,
	poaSeats poaseats.PoaSeats,
	vscBlocks vscBlocks.VscBlocks,
	balanceDb ledgerDb.Balances,
	da *datalayer.DataLayer,
	txCreator hive.HiveTransactionCreator,
	conf common.IdentityConfig,
	sconf systemconfig.SystemConfig,
	se *stateEngine.StateEngine,
	hiveConsumer *blockconsumer.HiveConsumer,
) ElectionProposer {
	return &electionProposer{
		conf:                conf,
		p2p:                 p2p,
		witnesses:           witnesses,
		elections:           elections,
		poaSeats:            poaSeats,
		vscBlocks:           vscBlocks,
		balanceDb:           balanceDb,
		da:                  da,
		txCreator:           txCreator,
		sconf:               sconf,
		se:                  se,
		hiveConsumer:        hiveConsumer,
		sigChannels:         make(map[uint64]chan *signResponse),
		scoreMapMinSamples:  resolveScoreMapMinSamples(sconf),
		banThresholdPercent: resolveBanThresholdPercent(sconf),
		sigCollectionWindow: resolveSigCollectionWindow(sconf),
	}
}

// resolveScoreMapMinSamples returns the per-instance ban-filter floor: the
// minimum per-witness block-signing opportunity before a witness can be banned.
// Production default is 500 blocks.
// VSC_ELECTION_SCOREMAP_MIN_SAMPLES is honoured ONLY on devnet (NetId
// "vsc-devnet") so testnet/mainnet operators cannot accidentally diverge
// the consensus-critical threshold across nodes. Devnet tests rely on the
// override to activate the ban filter within a runnable test horizon.
func resolveScoreMapMinSamples(sconf systemconfig.SystemConfig) uint64 {
	const defaultMinSamples = uint64(500)
	if sconf == nil || sconf.NetId() != "vsc-devnet" {
		return defaultMinSamples
	}
	v := os.Getenv("VSC_ELECTION_SCOREMAP_MIN_SAMPLES")
	if v == "" {
		return defaultMinSamples
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return defaultMinSamples
	}
	return n
}

// resolveBanThresholdPercent returns the per-instance ban threshold: a witness
// that signed fewer than this percent of the blocks it had the opportunity to
// sign (see computeBanScores) is excluded from the next committee. Production
// default is 75. Like the sample floor, VSC_ELECTION_BAN_THRESHOLD_PERCENT is
// honoured ONLY on devnet (NetId "vsc-devnet") so testnet/mainnet operators
// cannot accidentally diverge this consensus-critical threshold across nodes.
// Values outside 1..100 fall back to the default.
func resolveBanThresholdPercent(sconf systemconfig.SystemConfig) uint64 {
	const defaultPercent = uint64(67)
	if sconf == nil || sconf.NetId() != "vsc-devnet" {
		return defaultPercent
	}
	v := os.Getenv("VSC_ELECTION_BAN_THRESHOLD_PERCENT")
	if v == "" {
		return defaultPercent
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n < 1 || n > 100 {
		return defaultPercent
	}
	return n
}

// resolveSigCollectionWindow returns the BLS signature collection window for
// waitForSigs. Production default is 120s. VSC_ELECTION_SIG_COLLECTION_WINDOW
// is honoured ONLY on devnet so testnet/mainnet operators cannot accidentally
// diverge the window across nodes.
func resolveSigCollectionWindow(sconf systemconfig.SystemConfig) time.Duration {
	const defaultWindow = 120 * time.Second
	if sconf == nil || sconf.NetId() != "vsc-devnet" {
		return defaultWindow
	}
	v := os.Getenv("VSC_ELECTION_SIG_COLLECTION_WINDOW")
	if v == "" {
		return defaultWindow
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultWindow
	}
	return d
}

// Init implements aggregate.Plugin.
func (e *electionProposer) Init() error {
	e.hiveConsumer.RegisterBlockTick("election-proposer", e.blockTick, true)
	return nil
}

// Start implements aggregate.Plugin.
func (e *electionProposer) Start() *promise.Promise[any] {
	err := e.startP2P()
	if err != nil {
		return utils.PromiseReject[any](err)
	}
	return utils.PromiseResolve[any](nil)
}

// Stop implements aggregate.Plugin.
func (e *electionProposer) Stop() error {
	return e.stopP2P()
}

func (e *electionProposer) blockTick(bh uint64, headHeight *uint64) {
	e.electionMu.Lock()
	defer e.electionMu.Unlock()

	e.bh.Store(bh)
	e.headHeight = headHeight

	if e.canHold() {

		slotInfo := stateEngine.CalculateSlotInfo(bh)

		schedule := e.se.GetSchedule(slotInfo.StartHeight)

		//Select current slot as per consensus algorithm
		var witnessSlot *stateEngine.WitnessSlot
		for _, slot := range schedule {
			if slot.SlotHeight == slotInfo.StartHeight {
				witnessSlot = &slot
				break
			}
		}

		if witnessSlot != nil {
			if witnessSlot.Account == e.conf.Get().HiveUsername && bh%(5*common.CONSENSUS_SPECS.SlotLength) == 0 {
				e.HoldElection(bh)
			}
		}
	}
}

func (e *electionProposer) canHold() bool {
	if e.headHeight == nil {
		return false
	}
	bh := e.bh.Load()
	if *e.headHeight > 50 && bh < *e.headHeight-50 {
		return false
	}

	electionInterval := e.sconf.ConsensusParams().ElectionInterval
	if bh < electionInterval {
		return false // Too early in chain for elections
	}

	result, _ := e.elections.GetElectionByHeight(bh)

	if result.BlockHeight >= bh-electionInterval {
		return false
	}

	// Pendulum settlement gate. `result.Epoch` is the active (closing) epoch
	// N at e.bh; this proposer is about to propose epoch N+1, which embeds
	// the settlement record for epoch N. Composing epoch N's record needs
	// epoch N-1 already settled (ComposeRecord's PrevEpoch must be N-1 for a
	// continuous chain), so the gate requires latestSettled >= N-1.
	//
	// Gating on N (the old behaviour) deadlocked: epoch N is only settled
	// once epoch N+1 lands, so "N settled before proposing N+1" can never
	// be satisfied. Genesis path: closing epoch 0/1 have no prior settlement
	// to gate on, and `result.Epoch >= 2` also guards the unsigned `- 1`.
	if result.Epoch >= 2 && e.se != nil {
		if e.se.GetLatestSettledEpoch() < result.Epoch-1 {
			return false
		}
	}

	return true
}

func (e *electionProposer) GenerateElection() (elections.ElectionHeader, elections.ElectionData, error) {
	// TODO: get latest block

	return e.GenerateElectionAtBlock(e.bh.Load())
}

// Generates a raw election graph from local data
// electionStateWaitTimeout bounds how long GenerateElectionAtBlock waits for
// the state engine to flush the election anchor block before reading state.
// Matches the block-producer barrier (composeStateWaitTimeout).
const electionStateWaitTimeout = 2 * time.Second

// electionAnchorMaxAhead bounds how far above the local processed height an
// election anchor may be before we refuse to act on it, mirroring the
// block-producer guard (blockProducer.go). Without it, an unauthenticated peer
// could supply a far-future anchor via the signer path (ValidateMessage is
// allow-all pending the CP-0b/F5 auth fix), making the determinism barrier
// below block for the full timeout and tying up a pubsub handler slot per
// poisoned message.
const electionAnchorMaxAhead = 20

func (e *electionProposer) GenerateElectionAtBlock(
	blk uint64,
) (elections.ElectionHeader, elections.ElectionData, error) {
	// Reject a far-future anchor before blocking on it (defense-in-depth for the
	// barrier below). The proposer passes blk <= e.bh, and with the deterministic
	// canonical anchor (CP-0b) the signer's anchor is on-chain-derived; this
	// guard catches a signer that is simply far behind the canonical anchor
	// (abstain rather than block the handler for the full timeout). The
	// subtraction is underflow-safe because of the blk > e.bh guard.
	localBh := e.bh.Load()
	if blk > localBh && blk-localBh > electionAnchorMaxAhead {
		return elections.ElectionHeader{}, elections.ElectionData{}, fmt.Errorf("election block %d too far ahead of local processed height %d", blk, localBh)
	}

	// Determinism barrier (audit F1 / CP-0a). The election tick is registered
	// async=true (block-consumer), so it fires as a goroutine BEFORE
	// StateEngine.ProcessBlock flushes block `blk`. Without this wait, the
	// witness read below and the per-witness HIVE_CONSENSUS read in
	// GenerateFullElection can observe pre-flush state and produce an election
	// CID that diverges from peers whose state engines have caught up, so the
	// BLS aggregate never reaches quorum. Mirrors the block-producer barrier
	// (blockProducer.go). On timeout we return an error: a not-caught-up node
	// abstains from this election rather than signing/proposing a divergent CID
	// (same graceful degradation as any transient signer outage). NOTE: only
	// sufficient when `blk` is itself deterministic across nodes — pair with the
	// deterministic-anchor fix (CP-0b / F5); the in-memory anchor remains a
	// separate divergence source until then.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), electionStateWaitTimeout)
	if waitErr := e.se.WaitForProcessedHeight(waitCtx, blk); waitErr != nil {
		waitCancel()
		return elections.ElectionHeader{}, elections.ElectionData{}, fmt.Errorf("election state engine not caught up to block %d (lastProcessed=%d): %w", blk, e.se.LastProcessedHeight(), waitErr)
	}
	waitCancel()

	witnesses, werr := e.witnesses.GetWitnessesAtBlockHeight(blk, witnesses.EnabledOnly())
	if werr != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, werr
	}
	electionResult, elecErr := e.elections.GetElectionByHeight(blk - 1)
	if elecErr != nil && elecErr != mongo.ErrNoDocuments {
		return elections.ElectionHeader{}, elections.ElectionData{}, elecErr
	}

	var prevEpoch uint64
	var prevVer consensusversion.Version
	if elecErr == nil {
		prevEpoch = electionResult.Epoch
		prevVer = elections.ResultVersion(electionResult)
	}

	// TODO: Add a way to get the witness active score
	// const scoreChart = await this.self.witness.getWitnessActiveScore(blk)
	// scoreChart := map[string]uint64{}

	return e.GenerateFullElection(witnesses, prevEpoch, prevVer, blk)
}

const DEFAULT_NEW_NODE_WEIGHT = uint64(10)

var REQUIRED_ELECTION_MEMBERS = []string{
	// "vaultec.vsc",
} // TODO: Set this to a list of required election members

const VSC_ELECTION_TX_ID = "vsc.election_result"

// scoreMapMinSamples is the minimum per-witness opportunity — the number of
// blocks produced across the epochs a witness was actually elected to — that
// scoreMap must observe before that witness is eligible for the BannedNodes
// list. Below this many blocks (a freshly added or briefly-serving witness),
// the 75% participation threshold over a tiny denominator can mis-ban on noise,
// so we hold off and rely on later elections (with more of the witness's own
// history) to catch persistent delinquency.
//
// Resolved per-instance via resolveScoreMapMinSamples (see New). The default
// (500 blocks) is fixed on testnet and mainnet; only devnet honours the
// VSC_ELECTION_SCOREMAP_MIN_SAMPLES override.

// banSystemEnabled gates the per-witness participation ban filter applied in
// GenerateFullElection (see computeBanScores). It is currently DISABLED: even
// with the per-witness denominator fix, the filter was observed to gradually
// over-prune the committee — a witness that drops below the
// threshold is removed, then can no longer sign blocks to rebuild its score, so
// the network ratchets down to fewer and fewer nodes. Left off until a
// recovery-aware replacement lands; scoreMap/computeBanScores are kept intact
// so re-enabling is a one-line flip.
//
// MUST stay a compile-time constant. Disabling the ban changes which witnesses
// make the committee — and therefore the election CID — so every node has to
// make the identical decision. A per-node env/config toggle could diverge the
// committee across honest nodes and break the BLS aggregate; flip this (and
// rebuild the whole network) only in lockstep.
const banSystemEnabled = false

func (e *electionProposer) GenerateFullElection(
	witnessList []witnesses.Witness,
	previousEpoch uint64,
	prevVersion consensusversion.Version,
	blockHeight uint64,
) (elections.ElectionHeader, elections.ElectionData, error) {
	// The version floor carries forward from the previous election (major.consensus only)
	// and may rise to a scheduled target once the stake-readiness guard passes (below).
	floor := consensusversion.Version{Major: prevVersion.Major, Consensus: prevVersion.Consensus}

	witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		return !w.ConsensusVersionTriple().MeetsConsensusMin(floor)
	})

	// ensure the list is in a deterministic order
	slices.SortFunc(
		witnessList,
		func(a witnesses.Witness, b witnesses.Witness) int {
			return strings.Compare(a.Account, b.Account)
		},
	)

	// Strict consensus + gateway key admission (audit H-6), gated on the v0.2.0
	// activation via WitnessKeyStrictActive — keyed on the chain-active consensus
	// version carried in from the PRIOR ratified election (prevVersion), NOT a
	// height. Resolving from prevVersion keeps this gate out of the version-rise
	// readiness loop (no circular dependency with the floor advance below) and
	// gives the network one full epoch after the floor crosses 0.2.0 for witnesses
	// to (re-)announce a valid PoP before the gate bites. Below 0.2.0 (and on every
	// network whose floor has not reached it) this is skipped entirely — the legacy
	// warn-only-PoP, no-dedup behaviour is unchanged. At/after it:
	//   (a) EXCLUDE any witness whose consensus BLS key fails proof-of-possession
	//       — a rogue key the announcer does not hold the secret for (admitted
	//       today because the announce check is warn-only) can never reach the
	//       committee, closing the aggregate-signature forgery vector.
	//   (b) DEDUPE by consensus key — two accounts announcing the SAME key would
	//       double-count in the weighted BLS aggregate; keep the
	//       account-lexicographically-first (witnessList is already account
	//       sorted, so DeleteFunc's in-order pass keeps the first deterministically).
	// Pure functions of the on-chain witness records ⇒ identical committee/CID on
	// every node and signer (Constraint 3). The members loop below still skips an
	// unparseable consensus key as a final safety net.
	if consensusversion.WitnessKeyStrictActive(prevVersion) {
		seenConsensusKeys := make(map[string]string, len(witnessList))
		seenGatewayKeys := make(map[string]string, len(witnessList))
		witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
			if popErr := w.VerifyConsensusPoP(); popErr != nil {
				log.Warn("H-6: excluding witness with invalid consensus-key PoP",
					"account", w.Account, "err", popErr)
				return true
			}
			key, keyErr := w.ConsensusKey()
			if keyErr != nil {
				log.Warn("H-6: excluding witness with unparseable consensus key",
					"account", w.Account, "err", keyErr)
				return true
			}
			ks := key.String()
			if first, dup := seenConsensusKeys[ks]; dup {
				log.Warn("H-6: excluding witness with duplicate consensus key",
					"account", w.Account, "kept", first)
				return true
			}
			// Gateway-key strict admission (audit H-6, gateway companion): the
			// gateway key is an unauthenticated announced string unless its
			// proof-of-possession verifies. Without this, a distinct elected node
			// could announce ANOTHER member's public gateway key and wedge gateway
			// rotation with a duplicate key_auth (Hive rejects duplicate keys).
			//   (c) EXCLUDE any witness whose gateway-key PoP is missing/invalid —
			//       it has not proven it holds the announced gateway key.
			//   (d) DEDUPE by gateway key. With a valid PoP a duplicate implies a
			//       copied seed (also caught by the consensus-key dedup above for
			//       the normal derivation); this still covers a shared gateway key
			//       paired with distinct consensus keys. Keep the
			//       account-lexicographically-first (witnessList is account-sorted).
			// Pure function of the on-chain witness record ⇒ identical committee/CID
			// on every node (Constraint 3). After this gate every elected member
			// provably holds a unique gateway key, so the committee size and the
			// gateway multisig's signer count track the same population.
			if popErr := w.VerifyGatewayKeyPoP(); popErr != nil {
				log.Warn("H-6: excluding witness with invalid gateway-key PoP",
					"account", w.Account, "err", popErr)
				return true
			}
			if first, dup := seenGatewayKeys[w.GatewayKey]; dup {
				log.Warn("H-6: excluding witness with duplicate gateway key",
					"account", w.Account, "kept", first)
				return true
			}
			seenConsensusKeys[ks] = w.Account
			seenGatewayKeys[w.GatewayKey] = w.Account
			return false
		})
	}

	// Fetched before the POA seat gate below: the gate's starvation floor is
	// computed against the PRIOR committee size, the same basis the bond floor
	// guard uses.
	previousElection := e.elections.GetElection(previousEpoch)

	// ───── POA seat gate (A1) ─────
	//
	// Candidacy is restricted to accounts holding a ratified POA seat. Without
	// it, entry is permissionless: any Hive account can announce itself a
	// witness, and enough HIVE_CONSENSUS stake is the only remaining gate.
	//
	// PLACEMENT IS LOAD-BEARING. This runs BEFORE the stake/bond loop, the churn
	// cap and the bond floor guard, so the floor guard is still the LAST
	// membership-shrinking step and can still backfill incumbents if the seated
	// set comes up short. A membership gate placed after the floor guard is
	// exactly the shape that starved the mainnet committee at epoch 1699 (H-6
	// strict key admission, still disabled today).
	//
	// THREE INDEPENDENT SAFETY LAYERS, all required:
	//   1. version-gated on the PRIOR ratified election's version, so the gate
	//      bites one full epoch after the floor crosses 0.7.0;
	//   2. INERT while the registry is empty — bootstrap seeding fills it from
	//      the incumbent committee at the first ratified election after
	//      activation (state-processing/poa_seats.go), so an empty registry
	//      means "not seeded yet", never "nobody is eligible";
	//   3. FAIL-STOP on a read error — returning an error aborts this election
	//      attempt and the next slot retries, rather than proceeding with a
	//      partial seat set and deleting legitimate candidates.
	if consensusversion.PoaSeatGateActive(prevVersion) && e.poaSeats != nil {
		seats, seatErr := e.poaSeats.GetSeatsAtHeight(blockHeight)
		if seatErr != nil {
			return elections.ElectionHeader{}, elections.ElectionData{},
				fmt.Errorf("poa seat gate: registry read failed at %d: %w", blockHeight, seatErr)
		}
		if len(seats) == 0 {
			log.Error("poa seat gate ACTIVE but the seat registry is EMPTY — gate inert this election; expecting bootstrap seeding at the next ratified election",
				"block_height", blockHeight)
		} else {
			seatSet := make(map[string]struct{}, len(seats))
			for _, s := range seats {
				seatSet[s.Account] = struct{}{}
			}
			// Compute the gated list SPECULATIVELY first, so the starvation
			// guard below can decline to apply it.
			gated := slices.DeleteFunc(slices.Clone(witnessList), func(w witnesses.Witness) bool {
				// Witness accounts are bare; seat accounts are normalised on
				// write. Normalising here too means a future change to either
				// convention cannot silently empty the committee.
				_, seated := seatSet[poaseats.NormalizeAccount(w.Account)]
				return !seated
			})

			// ★ STARVATION GUARD. Everything else about this gate is a reason to
			// believe it will not empty the committee; this is the check that
			// makes it true regardless of whether those reasons hold.
			//
			// It exists because the failure it prevents has already happened
			// once: the structurally identical H-6 key-admission gate starved the
			// mainnet committee below the floor at epoch 1699 and halted
			// elections, and is still disabled today. A gate cannot be allowed to
			// be the thing that stops block production, so if applying it would
			// drop the committee under the floor that gateway rotation (>=8
			// keys), TSS signability and a valid election all require, it is NOT
			// applied at all — the election proceeds ungated and loudly, which
			// costs one epoch of permissionless candidacy instead of a stalled
			// chain.
			//
			// The concrete case this catches: a bootstrap that seeded only part
			// of the committee leaves a registry that is non-empty (so the
			// empty-registry guard above does not fire) but far too small.
			// MinMembers, NOT bondCommitteeFloor. bondCommitteeFloor folds in the
			// hardcoded 8-key gateway floor, which is right for the bond guard but
			// wrong here: on a 3-node devnet it exceeds the whole network, so the
			// gate could never apply anywhere and POA would be untestable. Mainnet
			// already pins MinMembers to 8 *because* of that gateway floor (see the
			// H-5 note in system-config), so using MinMembers gives 8 on mainnet
			// and 3 on the test nets — the correct bar on each. MinMembers is also
			// precisely the bar whose violation aborts HoldElection and stalls the
			// epoch, which is the outcome this guard exists to prevent.
			floor := e.sconf.ConsensusParams().MinMembers
			if len(gated) < floor {
				log.Error("poa seat gate REFUSED: applying it would starve the committee below the floor — election proceeds UNGATED this epoch. The seat registry is short (likely a partial bootstrap); resolve it before the consensus floor rises further",
					"block_height", blockHeight,
					"seats", len(seats),
					"candidates", len(witnessList),
					"after_gate", len(gated),
					"floor", floor)
			} else {
				before := len(witnessList)
				witnessList = gated
				if dropped := before - len(witnessList); dropped > 0 {
					log.Info("poa seat gate excluded unseated candidates",
						"block_height", blockHeight,
						"seats", len(seats),
						"candidates_before", before,
						"excluded", dropped)
				}
			}
		}
	}

	var etype string
	var firstElection bool
	if previousElection != nil {
		etype = previousElection.Type
	} else {
		etype = "initial"
		firstElection = true
	}

	// #24 + dilution fix: when banSystemEnabled, apply the BannedNodes filter
	// from scoreMap (currently OFF — see banSystemEnabled). Without this the
	// per-witness score is computed but never read, so persistent
	// under-participators stay eligible for the next committee. scoreMap scores
	// each witness against only the blocks of the epochs it was actually
	// elected to (see computeBanScores) and applies the scoreMapMinSamples floor
	// per-witness, so BannedNodes is already empty until there is enough
	// per-witness evidence — no separate global sample gate is needed here. We
	// still skip entirely on:
	//   - the genesis election (no prior history)
	//   - missing vscBlocks dependency (test harnesses)
	//   - any error reading scoreMap (degrade to the no-filter behaviour
	//     rather than blocking election production on a transient DB read)
	//
	// Determinism note: scoreMap reads vscBlocks.GetBlocksByElection and
	// elections.GetElection from the LOCAL DB. Verifiers in HandleMessage
	// rebuild the election header via the same path, so divergent block
	// indexing between proposer and verifier yields divergent CIDs and the BLS
	// aggregate fails (election retries next slot — safety preserved, liveness
	// affected). The per-witness scoreMapMinSamples floor caps the divergence
	// window. A cleaner long-term fix would have the proposer publish its
	// BannedNodes set alongside the election so verifiers re-derive against the
	// proposed input. Tracked as a follow-up.
	if banSystemEnabled && !firstElection && e.vscBlocks != nil {
		sm, smErr := e.scoreMap()
		if smErr != nil {
			log.Warn("scoreMap failed; skipping ban filter", "err", smErr)
		} else if len(sm.BannedNodes) > 0 {
			banned := make(map[string]bool, len(sm.BannedNodes))
			for _, n := range sm.BannedNodes {
				banned[n] = true
			}
			before := len(witnessList)
			witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
				if banned[w.Account] {
					log.Verbose("skipping banned witness", "account", w.Account, "score", sm.Map[w.Account], "denom", sm.Denoms[w.Account], "samples", sm.Samples)
					return true
				}
				return false
			})
			if len(witnessList) < before {
				log.Info("bannedNodes filter applied", "removed", before-len(witnessList), "samples", sm.Samples)
			}
		}
	}

	// Epoch this election will create (used to compare against the scheduled activation epoch).
	var newEpoch uint64
	if !firstElection {
		newEpoch = previousEpoch + 1
	}

	// Bond inclusion gate (audit CP-2b-ii / F10, plan v2 §13.1 Stage 2).
	// At/after the pinned activation height every witness's stake is read
	// through the maturity gate (min replayed hive_consensus over the trailing
	// window — bond_maturity.go) instead of the raw point-in-time snapshot, so
	// newly-acquired stake takes the full window before it can influence the
	// committee. Below the activation height — and on every network whose
	// activation param is 0 — the legacy raw read runs UNCHANGED, byte-for-byte
	// (replay-inertness: historical/ungated elections are untouched).
	//
	// Determinism (Constraint 3): the gate is a pure function of blockHeight,
	// on-chain reads barriered by GenerateElectionAtBlock (CP-0a), and
	// compile-time params — both the proposer (HoldElection) and every signer
	// (makeElection) re-derive the identical members/weights/CID. Any read
	// error fail-stops the whole attempt (F4/H-10): an error-as-zero would
	// evict an honest member on one node only and fork the CID.
	bondParams := e.sconf.ConsensusParams()
	bondActive := bondParams.BondInclusionActive(blockHeight)
	var bondReader balanceReplayReader
	var bondEffective map[string]int64
	// bondMatured holds the WINDOW-MATURED stake ONLY (before the established
	// additive floor), keyed by account. Used exclusively to rank NEW entrants
	// under the F6 churn cap (audit A6-1): ranking churn entrants by the
	// established-inflated bondEffective would let a recently-returned member's
	// already-ratified weight out-rank honestly-matured fresh stakers for the
	// limited new-member seats. Churn PRIORITY must reflect genuine maturation,
	// not the membership-eligibility floor. Membership/weight still use
	// bondEffective; only the churn ranking uses this.
	var bondMatured map[string]int64
	var bondEstablished map[string]bondEstablishedInfo
	if bondActive {
		if e.se == nil || e.se.LedgerState == nil {
			// A wiring/test-harness gap must never silently disable an active
			// gate (the dead-safety-param class) — fail the attempt instead.
			return elections.ElectionHeader{}, elections.ElectionData{},
				fmt.Errorf("bond inclusion gate active at %d but ledger state unavailable", blockHeight)
		}
		bondReader = e.se.LedgerState
		bondEffective = make(map[string]int64, len(witnessList))
		bondMatured = make(map[string]int64, len(witnessList))

		// Established-member exception (operator requirement): a witness that has
		// served as a ratified committee member keeps the stake it was already
		// ratified for exempt from the inclusion window, through up to the
		// absence grace, even if it drops out. Build the account→most-recent
		// membership map from the previous elections. CANNOT-RESET safety: if a
		// previous election exists but the history read comes back empty (a
		// transient DB read failure), proceeding would gate established members'
		// already-earned stake (a reset). Fail-stop instead — the next slot
		// retries with a healthy read. Skipped at genesis (no history) and when
		// the grace param is 0 (disabled).
		if bondParams.BondInclusionEstablishedGraceBlocks > 0 && !firstElection {
			lookback := bondEstablishedLookback(bondParams.BondInclusionEstablishedGraceBlocks, bondParams.ElectionInterval)
			prevs := e.elections.GetPreviousElections(newEpoch, lookback)
			if len(prevs) == 0 && previousElection != nil {
				return elections.ElectionHeader{}, elections.ElectionData{},
					fmt.Errorf("bond established: previous-election history read returned empty despite an existing previous election (epoch %d)", previousEpoch)
			}
			bondEstablished = buildBondEstablishedMap(prevs)
		}
	}

	stakedMap := map[string]uint64{}
	defaultWeightMap := map[string]uint64{}
	nodesWithStake := uint64(0)
	for _, w := range witnessList {
		if bondActive {
			// F10 single canonical matured-set pass: this effective value is the
			// ONLY stake read for this witness — membership, nodesWithStake (the
			// staked/initial type decision) and the version-readiness denominator
			// below all derive from the same stakedMap/weightMap it fills. No
			// second raw read anywhere in the gated path.
			matured, mErr := maturedConsensusStake(
				bondReader,
				"hive:"+w.Account,
				blockHeight,
				bondParams.BondInclusionWindowBlocks,
				bondParams.BondInclusionSampleCount,
			)
			if mErr != nil {
				return elections.ElectionHeader{}, elections.ElectionData{},
					fmt.Errorf("bond inclusion gate: maturity read failed for %s: %w", w.Account, mErr)
			}
			eff := matured
			// Record the matured-only value for the churn-cap ranking (A6-1)
			// BEFORE the additive established floor raises eff.
			bondMatured[w.Account] = matured
			// The established-member exception is a min(current, cap) floor that
			// can only RAISE eff (never reset a member): a witness that recently
			// served keeps its already-ratified stake exempt from the window.
			estInfo, hasEst := bondEstablished[w.Account]
			if bondWithinEstablishedGrace(estInfo, hasEst, blockHeight, bondParams.BondInclusionEstablishedGraceBlocks) {
				cur, cErr := maturedConsensusStake(bondReader, "hive:"+w.Account, blockHeight, 0, 0)
				if cErr != nil {
					return elections.ElectionHeader{}, elections.ElectionData{},
						fmt.Errorf("bond inclusion gate: point-in-time read failed for %s: %w", w.Account, cErr)
				}
				// Stake the member was already ratified for stays exempt from the
				// window (capped at min(current, last-ratified)) through the
				// absence grace. Purely additive — never lowers eff.
				if est := bondEstablishedExemption(estInfo, hasEst, blockHeight, bondParams.BondInclusionEstablishedGraceBlocks, cur); est > eff {
					eff = est
				}
			}
			bondEffective[w.Account] = eff
			// Same H-8 shape as the raw branch: strictly positive before the
			// uint64 cast (maturedConsensusStake already floors negatives to 0,
			// this keeps the cast locally provable-safe).
			if eff > 0 && eff >= bondParams.MinStake {
				nodesWithStake++
				stakedMap[w.Account] = uint64(eff)
			}
			defaultWeightMap[w.Account] = DEFAULT_NEW_NODE_WEIGHT
			continue
		}
		balRecord, err := e.balanceDb.GetBalanceRecord("hive:"+w.Account, blockHeight)
		if err != nil {
			return elections.ElectionHeader{}, elections.ElectionData{}, err
		}
		if balRecord != nil {
			// H-8 defense-in-depth: HIVE_CONSENSUS is int64 and is cast to uint64
			// below. Require strictly positive so a negative balance can never
			// wrap to a ~1.8e19 governance weight even if MinStake were ever
			// mis-set to <= 0 (there is no ConsensusParams.Validate() — audit
			// m18). At current params (MinStake=2,000,000) the >= MinStake check
			// already excludes negatives; this makes the cast safe regardless.
			if balRecord.HIVE_CONSENSUS > 0 && balRecord.HIVE_CONSENSUS >= e.sconf.ConsensusParams().MinStake {
				nodesWithStake++
				stakedMap[w.Account] = uint64(balRecord.HIVE_CONSENSUS)
			}
		}
		defaultWeightMap[w.Account] = DEFAULT_NEW_NODE_WEIGHT
	}

	var pType string
	weightMap := map[string]uint64{}
	if nodesWithStake >= uint64(e.sconf.ConsensusParams().MinMembers) || etype == "staked" {
		pType = "staked"
		weightMap = stakedMap

		// ───── POA flat seat-weight (A3) ─────
		//
		// Every ratified seat carries exactly one unit. Stake keeps every other
		// role it has — the MinStake eligibility floor, the maturity window, the
		// established-member grace, and being the slashable bond — but it stops
		// being consensus WEIGHT. That is the whole point: with weight tracking
		// stake 1:1, an account holding >=2/3 of stake holds >=2/3 of forging
		// weight and of the gateway multisig, which is the capture vector POA
		// exists to close. Flat weight makes "2/3" mean 14 seats of 20.
		//
		// This is not a novel code path: pType=="initial" already gives every
		// member a flat DEFAULT_NEW_NODE_WEIGHT today, and every downstream
		// consumer is a ratio rule over whatever weights the election carries —
		// none parse weight VALUES. TSS keygen/keysign thresholds are member
		// COUNT based (tss_helpers.GetThreshold) and are untouched.
		//
		// pType deliberately stays "staked": it is consensus-visible (part of
		// ElectionData and therefore of the CID) and feeds elections.ResultVersion,
		// so inventing a third value would ripple into election validation for no
		// benefit.
		if e.poaActive(prevVersion) {
			flat := make(map[string]uint64, len(stakedMap))
			for account := range stakedMap {
				flat[account] = params.PoaSeatWeight
			}
			weightMap = flat
		}
	} else {
		pType = "initial"
		weightMap = defaultWeightMap
	}

	// Snapshot the pre-delete (enabled + version-eligible + unbanned) witness
	// set for the bond floor guard below: only witnesses evicted BY THE GATE —
	// not by any other filter — are backfill candidates. Cloned because
	// DeleteFunc mutates the backing array.
	var bondPreGate []witnesses.Witness
	if bondActive && pType == "staked" {
		bondPreGate = slices.Clone(witnessList)
	}

	witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		weight, included := weightMap[w.Account]
		return !included || weight == 0
	})

	// Config-pinned version floor (simple rollout path): at/after the configured epoch the
	// floor rises to the configured target WITHOUT an on-chain proposal or stake-readiness
	// guard. PinnedVersionFloor is a pure function of network config + newEpoch, so every
	// signer regenerating this election resolves the identical floor → identical members →
	// identical CID. The propose/activation block below stays dormant unless a proposal is
	// actually posted; this pin and that block compose (floor only ever rises).
	if pinned := e.sconf.ConsensusParams().PinnedVersionFloor(newEpoch); pinned.Cmp(floor) > 0 {
		floor = pinned
		witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
			return !w.ConsensusVersionTriple().MeetsConsensusMin(floor)
		})
	}

	// Epoch-scheduled version switch with stake-readiness guard. Resolved here (at the
	// ratified election checkpoint) rather than via a live singleton, so every signer
	// regenerating this election at the same height computes the identical floor → CID.
	// The schedule is read height-addressably (ScheduledActivationForHeight) for purity.
	var schedule *consensus_state.ScheduledActivation
	if e.se != nil {
		schedule = e.se.ScheduledActivationForHeight(blockHeight)
	}
	if schedule != nil &&
		newEpoch >= schedule.ActivationEpoch && schedule.Target().Cmp(floor) > 0 {
		target := schedule.Target()
		var totalWeight, readyWeight uint64
		for _, w := range witnessList {
			wt := weightMap[w.Account]
			totalWeight += wt
			if w.ConsensusVersionTriple().MeetsConsensusMin(target) {
				readyWeight += wt
			}
		}
		num, den := int64(
			e.sconf.ConsensusParams().ConsensusVersionActivationNum,
		), int64(
			e.sconf.ConsensusParams().ConsensusVersionActivationDen,
		)
		if num <= 0 || den <= 0 {
			num, den = 4, 5 // default 80%
		}

		// H-3/C-2 (TSS vault-freeze prevention): the 80% guard above only proves
		// the NEW committee is ready for the target. But the OUTGOING committee
		// holds the outstanding TSS key shares, and BOTH signing AND resharing
		// those keys are gated by this same version floor (tss.go:1104/1310) — so
		// advancing the floor past the outgoing committee's readiness can filter
		// its share-holders below threshold, freezing the BTC/ETH/DASH vault
		// (can't sign, can't reshare to hand off). Additionally require the
		// previous committee to retain threshold+1 (= ceil(2N/3), the
		// GetThreshold+1 quorum) members that meet the target. Conservative +
		// deterministic: a previous member who is no longer a current candidate,
		// or isn't on the target version, counts as not-ready — which can only
		// BLOCK the advance (the floor simply waits another epoch; a delayed
		// version upgrade is never a custody freeze). Skipped at genesis (no
		// previous committee) and when Forced (recovery override accepts the
		// risk explicitly). Pure function of previousElection (on-chain) + the
		// deterministic candidate set + compile-time target ⇒ identical on every
		// signer (Constraint 3).
		prevCommitteeReady := true
		if !schedule.Forced && previousElection != nil && len(previousElection.Members) > 0 {
			candidateVer := make(map[string]consensusversion.Version, len(witnessList))
			for _, w := range witnessList {
				candidateVer[w.Account] = w.ConsensusVersionTriple()
			}
			prevReady := 0
			for _, m := range previousElection.Members {
				acct := strings.TrimPrefix(m.Account, "hive:")
				if v, ok := candidateVer[acct]; ok && v.MeetsConsensusMin(target) {
					prevReady++
				}
			}
			// threshold+1 = ceil(2N/3) = (2N+2)/3 (matches tss_helpers.GetThreshold(N)+1).
			prevQuorum := (2*len(previousElection.Members) + 2) / 3
			if prevReady < prevQuorum {
				prevCommitteeReady = false
				log.Warn("H-3/C-2: deferring version-floor advance — outgoing committee below TSS-reshare quorum at target",
					"block_height", blockHeight, "prev_ready", prevReady, "prev_quorum", prevQuorum, "target", target.Format())
			}
		}

		// Integer-only comparison: readyWeight/totalWeight >= num/den.
		if schedule.Forced || (prevCommitteeReady && totalWeight > 0 && int64(readyWeight)*den >= int64(totalWeight)*num) {
			floor = target
			witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
				return !w.ConsensusVersionTriple().MeetsConsensusMin(floor)
			})
		}
	}

	// Bond churn cap (audit F6 / THORChain NumberOfNewNodesPerChurn): the
	// maturity window staggers INDIVIDUAL stakers, but a coordinated cohort that
	// all matures on the same boundary would enter atomically in ONE election —
	// a majority flip with no per-epoch reaction window. Cap the number of NEW
	// members (accounts not in the previous ratified election) admitted per
	// election to MaxNewMembersPerElection, keeping the highest-effective subset
	// and deferring the rest to later elections (they re-compete next epoch — the
	// backlog drains deterministically). Runs BEFORE the floor guard so capping
	// is a rate limit on NEW entry while the floor guard still restores
	// viability from INCUMBENTS (which the cap never touches — incumbents are
	// not "new", so the two cannot fight). Deterministic: new-member set is the
	// on-chain previous election; ranking is the already-computed effective
	// values; cap is compile-time. Inert unless bondActive, staked, cap>0, and
	// there is a previous election (genesis/bootstrap has no "new" concept).
	effMaxNew := e.sconf.ConsensusParams().EffectiveMaxNewMembers(blockHeight)

	// ───── POA churn-cap activation (A4) ─────
	//
	// The cap above is DEAD CODE on every shipped network: EffectiveMaxNewMembers
	// requires MaxNewMembersActivationHeight to be pinned and reached, and it is
	// 0 everywhere — including mainnet, where MaxNewMembersPerElection is 1 but
	// the height is unset. (Four devnet tests set the cap value and assert on it
	// while the height stays 0, so they exercise an inert cap.)
	//
	// POA activates it off the version gate instead of pinning a height. That
	// removes the mis-pin footgun documented on the field itself: a bare value
	// lets old-binary (cap=0) and new-binary (cap=N) nodes compute DIFFERENT
	// member sets for the same election, which breaks BLS aggregation and stalls
	// the epoch. A version gate cannot desynchronise that way, because the floor
	// only rises once a supermajority attests to running the code.
	//
	// A pinned MaxNewMembersActivationHeight still wins where present: this only
	// supplies a cap when the height-gated path yields none.
	poaChurn := e.poaActive(prevVersion)
	if poaChurn && effMaxNew == 0 {
		effMaxNew = e.sconf.ConsensusParams().EffectivePoaMaxNewMembers()
	}

	// The pre-existing guard also required bondActive, because the cap was
	// meaningful only alongside the bond maturity gate. Under POA the cap is
	// meaningful on its own — it rate-limits SEATS entering the committee, which
	// is what makes a suspicious admission wave visible and reactable instead of
	// atomic — so POA admits it independently of bondActive.
	if (bondActive || poaChurn) && pType == "staked" && previousElection != nil && effMaxNew > 0 {
		prevMembers := make(map[string]struct{}, len(previousElection.Members))
		for _, m := range previousElection.Members {
			prevMembers[strings.TrimPrefix(m.Account, "hive:")] = struct{}{}
		}
		newEntrants := make([]bondChurnEntrant, 0, len(witnessList))
		for _, w := range witnessList {
			if _, wasMember := prevMembers[w.Account]; wasMember {
				continue
			}
			// Established-member exception: a returning established member (within
			// the absence grace) is NOT a "new" entrant — they already earned
			// their place — so the churn cap must not defer them ("once they're
			// in, they aren't touched"). They re-enter immediately, like an
			// incumbent. Safe: their stake is capped at last-ratified, no new
			// power.
			if estInfo, ok := bondEstablished[w.Account]; bondWithinEstablishedGrace(estInfo, ok, blockHeight, e.sconf.ConsensusParams().BondInclusionEstablishedGraceBlocks) {
				continue
			}
			newEntrants = append(newEntrants, bondChurnEntrant{
				account: w.Account,
				// A6-1: rank by MATURED-only stake, not the grandfather/established
				// -inflated bondEffective, so fresh unmatured capital (even wearing
				// an old activation-era grandfather weight) cannot jump the churn
				// queue ahead of honestly-matured stakers.
				effective: bondMatured[w.Account],
			})
		}
		churnedOut := selectChurnedOut(newEntrants, effMaxNew)
		if len(churnedOut) > 0 {
			witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
				_, drop := churnedOut[w.Account]
				return drop
			})
			deferred := make([]string, 0, len(churnedOut))
			for acct := range churnedOut {
				deferred = append(deferred, acct)
			}
			slices.Sort(deferred)
			log.Info("bond churn cap deferred new members",
				"block_height", blockHeight,
				"cap", effMaxNew,
				"new_entrants", len(newEntrants),
				"deferred", strings.Join(deferred, ","))
		}
	}

	// Bond floor guard (audit C-2/C-3, plan v2 §13.1 Stage 1 item 5): the gate
	// must never shrink the committee below what gateway rotation (≥8 keys),
	// TSS signability (prior-committee engagement) and a valid election
	// (MinMembers — HoldElection aborts below it, stalling the epoch) need.
	// When the matured set is short, re-seat prior-election incumbents — and
	// ONLY incumbents (R3/H8: a guard that admits arbitrary unmatured stake is
	// itself an attack lever: thin the committee, force your fresh nodes in) —
	// each capped at min(current balance, previously-ratified weight), best
	// effort (an incumbent who genuinely unstaked below MinStake is never
	// re-seated; if candidates run out the committee stays short and the
	// existing MinMembers abort handles it).
	//
	// Placement (scrutiny fix #1): this guard runs AFTER both version-floor
	// deletes above — i.e. it is the LAST membership-shrinking step — so the
	// floor it asserts actually holds in the emitted election. Candidates must
	// therefore meet the FINAL resolved version floor (a backfill that the
	// version filter would have deleted is not a seat) and must carry a
	// parseable consensus key (the members loop below deterministically skips
	// unparseable keys, so such a witness cannot fill a seat either). The
	// version-readiness denominator above intentionally uses the MATURED set
	// only — backfilled seats are a liveness patch, not readiness evidence.
	// Deterministic: candidates, caps and ordering are pure functions of the
	// ratified previous election + barriered replay reads (Constraint 3).
	if bondActive && pType == "staked" && previousElection != nil && len(previousElection.Members) > 0 {
		bondFloor := bondCommitteeFloor(e.sconf.ConsensusParams().MinMembers, len(previousElection.Members))
		if len(witnessList) < bondFloor {
			seated := make(map[string]struct{}, len(witnessList))
			for _, w := range witnessList {
				seated[w.Account] = struct{}{}
			}
			prevWeights := make(map[string]uint64, len(previousElection.Members))
			for i, m := range previousElection.Members {
				if i >= len(previousElection.Weights) {
					break
				}
				prevWeights[strings.TrimPrefix(m.Account, "hive:")] = previousElection.Weights[i]
			}
			cands := make([]bondBackfillCandidate, 0, len(bondPreGate))
			for _, w := range bondPreGate {
				if _, in := seated[w.Account]; in {
					continue
				}
				if !w.ConsensusVersionTriple().MeetsConsensusMin(floor) {
					continue
				}
				if _, keyErr := w.ConsensusKey(); keyErr != nil {
					continue
				}
				prevW, incumbent := prevWeights[w.Account]
				if !incumbent || prevW == 0 {
					continue
				}
				cur, cErr := maturedConsensusStake(bondReader, "hive:"+w.Account, blockHeight, 0, 0)
				if cErr != nil {
					return elections.ElectionHeader{}, elections.ElectionData{},
						fmt.Errorf("bond floor guard: balance read failed for %s: %w", w.Account, cErr)
				}
				wgt := min(cur, uint64ToInt64Clamped(prevW))
				if wgt <= 0 || wgt < e.sconf.ConsensusParams().MinStake {
					continue
				}
				cands = append(cands, bondBackfillCandidate{
					witness:    w,
					weight:     uint64(wgt),
					effective:  bondEffective[w.Account],
					prevWeight: prevW,
				})
			}
			selected := selectBondBackfill(cands, bondFloor-len(witnessList))
			for _, c := range selected {
				witnessList = append(witnessList, c.witness)
				// A backfilled incumbent is a seat like any other. Under POA it
				// must carry the SAME flat weight as everyone else: seating it
				// at its stake-derived weight would reintroduce a
				// stake-proportional member into an otherwise flat committee —
				// a single member whose weight could dwarf every other seat
				// combined, i.e. the exact capture vector A3 removes, restored
				// through the liveness patch.
				if e.poaActive(prevVersion) {
					weightMap[c.witness.Account] = params.PoaSeatWeight
				} else {
					weightMap[c.witness.Account] = c.weight
				}
			}
			if len(selected) > 0 {
				// Restore the deterministic account ordering the members array
				// (and therefore the election CID) depends on.
				slices.SortFunc(witnessList, func(a witnesses.Witness, b witnesses.Witness) int {
					return strings.Compare(a.Account, b.Account)
				})
				backfilled := make([]string, 0, len(selected))
				for _, c := range selected {
					backfilled = append(backfilled, c.witness.Account)
				}
				log.Info("bond floor guard re-seated prior-election incumbents",
					"block_height", blockHeight,
					"floor", bondFloor,
					"matured_committee", len(witnessList)-len(selected),
					"backfilled", strings.Join(backfilled, ","))
			}
		}
	}

	optionalNodes := slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		return slices.Contains(REQUIRED_ELECTION_MEMBERS, w.Account)
	})

	totalOptionalWeight := utils.Sum(
		utils.Map(optionalNodes, func(w witnesses.Witness) uint64 {
			return weightMap[w.Account]
		}),
	)

	distWeight := computeRequiredMemberWeight(totalOptionalWeight, uint64(len(REQUIRED_ELECTION_MEMBERS)))

	// REQUIRED_ELECTION_MEMBERS is empty today, so distWeight is inert — but it
	// is a weight computed as a SHARE OF THE TOTAL, which under flat seat-weight
	// would hand a required member many times a normal seat's weight the moment
	// the list is ever populated. Flattening it here means POA's one-seat-one-vote
	// property cannot be quietly undone later by adding a required member.
	if e.poaActive(prevVersion) {
		distWeight = params.PoaSeatWeight
	}

	// review2 MEDIUM #66: a witness consensus key comes from untrusted L1
	// account data; a single malformed key must not panic (crash) every
	// proposer. Deterministically skip the witness instead — all nodes
	// read the same witness records, so dropping the same bad-key members
	// keeps the proposed election identical across the network.
	members := make([]elections.ElectionMember, 0, len(witnessList))
	for _, w := range witnessList {
		key, err := w.ConsensusKey()
		if err != nil {
			log.Warn("skipping witness with invalid consensus key", "account", w.Account, "err", err)
			continue
		}
		t := w.ConsensusVersionTriple()
		members = append(members, elections.ElectionMember{
			Key:                 key.String(),
			Account:             w.Account,
			HasPerMemberVersion: true,
			MemberMajor:         t.Major,
			MemberConsensus:     t.Consensus,
			MemberNonConsensus:  t.NonConsensus,
		})
	}

	weights := make([]uint64, len(members))
	for i, member := range members {
		if slices.Contains(REQUIRED_ELECTION_MEMBERS, member.Account) {
			weights[i] = distWeight
		} else {
			weights[i] = weightMap[member.Account]
		}
	}

	electionData := elections.ElectionData{}

	if firstElection {
		electionData.Epoch = 0
	} else {
		electionData.Epoch = previousEpoch + 1
	}
	electionData.Members = members
	electionData.NetId = e.sconf.NetId()
	electionData.ProtocolVersion = floor.Consensus
	electionData.VersionMajor = floor.Major
	// non_consensus is informational and not coordinated at the election level.
	electionData.VersionNonConsensus = 0
	electionData.Weights = weights
	electionData.Type = pType

	// Pendulum settlement (inlined). Skipped on the genesis election because
	// there's no closing committee to settle. On any other epoch we compose
	// the settlement record for `previousEpoch` (the epoch this election is
	// rotating away from) against the same blockHeight the election itself
	// uses, so every signer's re-derive sees identical inputs. ComposeRecord
	// failure aborts the election attempt — the proposer returns the error
	// without broadcasting, and the next slot's leader retries.
	if !firstElection && e.se != nil && previousElection != nil && previousEpoch > 0 {
		settlementMembers := make([]string, 0, len(previousElection.Members))
		for _, m := range previousElection.Members {
			acct := m.Account
			if !strings.HasPrefix(acct, "hive:") {
				acct = "hive:" + acct
			}
			settlementMembers = append(settlementMembers, acct)
		}

		balanceReader := e.se.PendulumBalanceRecordReader()
		if balanceReader == nil {
			return elections.ElectionHeader{}, elections.ElectionData{},
				fmt.Errorf("pendulum settlement: balance reader unavailable")
		}
		bonds, bondsErr := pendulumsettlement.ReadCommitteeBonds(
			balanceReader,
			settlementMembers,
			previousElection.BlockHeight,
			blockHeight,
		)
		if bondsErr != nil {
			// Fail-stop (audit GAP-1): a non-deterministic bond-read error must
			// abort this election attempt (the next slot's leader retries)
			// rather than embed a divergent settlement → CID fork.
			return elections.ElectionHeader{}, elections.ElectionData{},
				fmt.Errorf("pendulum settlement: committee bond read failed: %w", bondsErr)
		}
		bucket := e.se.PendulumNodesBucketBalance(blockHeight)
		prevSettled := e.se.GetLatestSettledEpoch()
		tickInterval := e.se.PendulumOracleTickInterval()

		var reductions map[string]int
		if provider := e.se.PendulumEpochInputsProvider(); provider != nil {
			reductions = rewards.ComputeReductionsForEpoch(
				provider,
				previousElection.BlockHeight,
				blockHeight,
				tickInterval,
			)
		}

		rec, composeErr := pendulumsettlement.ComposeRecord(pendulumsettlement.ComposeInputs{
			Epoch:               previousEpoch,
			PrevEpoch:           prevSettled,
			EpochStartBh:        previousElection.BlockHeight,
			SlotHeight:          blockHeight,
			CommitteeBonds:      bonds,
			BucketBalanceHBD:    bucket,
			ReductionsByAccount: reductions,
		})
		if composeErr != nil {
			return elections.ElectionHeader{}, elections.ElectionData{},
				fmt.Errorf("pendulum settlement: compose failed: %w", composeErr)
		}
		if rec == nil {
			return elections.ElectionHeader{}, elections.ElectionData{},
				fmt.Errorf("pendulum settlement: compose returned nil record")
		}
		electionData.Settlement = rec
	}

	cid, err := electionData.Cid()
	if err != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}

	cborNode, _ := electionData.Node()
	ses := datalayer.NewSession(e.da)

	ses.Put(cborNode.RawData(), cid)
	ses.Commit()

	electionHeader := elections.ElectionHeader{}
	electionHeader.Data = cid.String()
	electionHeader.Epoch = electionData.Epoch
	electionHeader.NetId = electionData.NetId
	electionHeader.Type = electionData.Type

	return electionHeader, electionData, nil
}

// canonicalElectionAnchor returns the deterministic block height at which the
// election following prevBlockHeight must be generated: the first
// election-trigger block (a multiple of 5*SlotLength, matching the blockTick
// trigger and the canHold interval gate) at or after prevBlockHeight +
// electionInterval. It is a pure function of the on-chain prevBlockHeight and
// compile-time consensus params, so every proposer and every signer computes
// the identical anchor — making the embedded settlement record (SnapshotRangeTo
// = anchor) and the election CID byte-identical across nodes and across
// consecutive slot retries, with no in-memory anchor to lose on restart or to
// poison via an unauthenticated sign_request (audit F5 / CP-0b). For the first
// proposer of an epoch the result equals its trigger block; a later retry
// proposer recomputes the same value instead of remembering it.
func canonicalElectionAnchor(prevBlockHeight uint64, electionInterval uint64) uint64 {
	cadence := 5 * common.CONSENSUS_SPECS.SlotLength
	if cadence == 0 {
		cadence = 1
	}
	// canHold fires the proposer at the first trigger block STRICTLY greater
	// than prevBlockHeight + electionInterval (canHold returns false while
	// prevBh >= e.bh-interval, i.e. it proceeds only once e.bh > prevBh+interval,
	// election-proposer.go canHold). Use +1 so the canonical anchor equals that
	// first trigger block in ALL cases — including when prevBh+interval is itself
	// a multiple of the cadence — rather than landing one cadence early.
	target := prevBlockHeight + electionInterval + 1
	return ((target + cadence - 1) / cadence) * cadence
}

type ElectionOptions struct {
	OverrideMinimumMemberCount int
}

func (ep *electionProposer) HoldElection(blk uint64, options ...ElectionOptions) error {
	if ep.signingInfo != nil {
		log.Warn("election already in progress; skipping new attempt", "block_height", blk)
		return errors.New("election already in progress")
	}

	electionResult, err := ep.elections.GetElectionByHeight(blk - 1)
	firstElection := false
	if err == mongo.ErrNoDocuments {
		firstElection = true
	} else if err != nil {
		log.Error("GetElectionByHeight failed", "block_height", blk, "err", err)
		return err
	}

	// Canonical anchor (audit F5 / CP-0b): derive the election anchor
	// deterministically from the on-chain previous-election height rather than
	// from an in-memory first-writer-wins map. The first proposer for this epoch
	// triggers exactly at canonicalElectionAnchor (canHold gates on the same
	// ElectionInterval and the blockTick trigger fires on the same 5*SlotLength
	// cadence), and any retry proposer recomputes the identical value — so the
	// settlement record (SnapshotRangeTo = anchorBh) and the election CID are
	// byte-identical across nodes and across retries, restart-safe, and not
	// poisonable via an unauthenticated sign_request.
	anchorBh := blk
	if !firstElection {
		anchorBh = canonicalElectionAnchor(electionResult.BlockHeight, ep.sconf.ConsensusParams().ElectionInterval)
		if anchorBh != blk {
			log.Info("using canonical election anchor",
				"proposer_bh", blk,
				"anchor_bh", anchorBh,
				"epoch", electionResult.Epoch+1)
		}
	}

	electionHeader, electionData, err := ep.GenerateElectionAtBlock(anchorBh)

	if err != nil {
		log.Error("GenerateElectionAtBlock failed", "block_height", anchorBh, "err", err)
		return err
	}

	if firstElection {
		electionResultJson := struct {
			elections.ElectionHeader
		}{
			electionHeader,
		}
		jsonBytes, err := json.Marshal(electionResultJson)
		if err != nil {
			log.Error(
				"marshal first-election broadcast failed",
				"block_height",
				blk,
				"epoch",
				electionHeader.Epoch,
				"err",
				err,
			)
			return err
		}

		op := ep.txCreator.CustomJson(
			[]string{ep.conf.Get().HiveUsername},
			[]string{},
			VSC_ELECTION_TX_ID,
			string(jsonBytes),
		)

		tx := ep.txCreator.MakeTransaction([]hivego.HiveOperation{op})

		ep.txCreator.PopulateSigningProps(&tx, nil)

		sig, err := ep.txCreator.Sign(tx)
		if err != nil {
			log.Error("sign first-election tx failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
			return fmt.Errorf("failed to update account: %w", err)
		}

		tx.AddSig(sig)

		txId, err := ep.txCreator.Broadcast(tx)
		if err != nil {
			return err
		}

		log.Info("first election proposed", "block_height", blk, "epoch", electionHeader.Epoch, "tx_id", txId)

		return nil
	} else {

		// console.log("electionData - holding election", electionData)

		var minimumMemberCount int
		if len(options) > 0 {
			minimumMemberCount = options[0].OverrideMinimumMemberCount
		} else {
			minimumMemberCount = ep.sconf.ConsensusParams().MinMembers
		}

		if len(electionData.Members) < minimumMemberCount {
			log.Warn("election minimum member count not met",
				"block_height", blk,
				"epoch", electionHeader.Epoch,
				"members", len(electionData.Members),
				"minimum", minimumMemberCount)
			return errors.New("Minimum network config not met for election. Skipping.")
		}

		cid, err := electionHeader.Cid()
		if err != nil {
			log.Error("compute election header CID failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
			return err
		}

		memberAccounts := make([]string, 0, len(electionData.Members))
		for _, m := range electionData.Members {
			memberAccounts = append(memberAccounts, m.Account)
		}
		log.Verbose("election header built",
			"block_height", blk,
			"epoch", electionHeader.Epoch,
			"cid", cid.String(),
			"data_cid", electionHeader.Data,
			"member_count", len(electionData.Members),
			"members", strings.Join(memberAccounts, ","))

		var memberKeys []dids.BlsDID
		if firstElection {
			w, err := ep.witnesses.GetWitnessesAtBlockHeight(blk)

			if err != nil {
				log.Error("GetWitnessesAtBlockHeight failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
				return err
			}
			res := resultJoin(utils.Map(w, func(w witnesses.Witness) result.Result[dids.BlsDID] {
				return resultWrap(w.ConsensusKey())
			})...)

			if res.IsErr() {
				return res.UnwrapErr()
			}
			memberKeys = res.Unwrap()
		} else {
			memberKeys = electionResult.MemberKeys()
		}

		circuit, err := dids.NewBlsCircuitGenerator(memberKeys).Generate(cid)
		if err != nil {
			log.Error("generate BLS circuit failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
			return err
		}

		ep.signingInfo = &struct {
			epoch   uint64
			block   uint64
			cid     string
			circuit *dids.PartialBlsCircuit
		}{
			epoch:   electionHeader.Epoch,
			block:   blk,
			cid:     cid.String(),
			circuit: &circuit,
		}

		// sig, err := signCid(ep.conf, cid)

		// if err != nil {
		// 	return err
		// }

		signReq := signRequest{
			Epoch:       electionHeader.Epoch,
			BlockHeight: anchorBh,
		}

		reqJson, _ := json.Marshal(signReq)
		go func() {
			time.Sleep(4 * time.Millisecond)
			ep.service.Send(p2pMessage{
				Type: "sign_request",
				Op:   "hold_election",
				Data: string(reqJson),
			})
		}()

		ep.sigChannelsMu.Lock()
		// Buffered by member count (H-15 follow-up): an unbuffered channel makes
		// a pubsub sender block forever if the collector already returned (quorum
		// reached or timeout), leaking a goroutine + a pubsub semaphore slot per
		// late sign_response — 256 such leaks wedge the topic. One slot per
		// possible signer means no send ever blocks. memberKeys (one BLS DID per
		// member) is the signer count in scope here.
		ep.sigChannels[ep.signingInfo.epoch] = make(chan *signResponse, len(memberKeys))
		ep.sigChannelsMu.Unlock()

		log.Info("waiting for signatures",
			"block_height", anchorBh,
			"epoch", electionHeader.Epoch,
			"timeout", ep.sigCollectionWindow)
		ctx, cancel := context.WithTimeout(context.Background(), ep.sigCollectionWindow)
		defer cancel()
		signedWeight, err := ep.waitForSigs(ctx, &electionResult)
		ep.sigChannelsMu.Lock()
		delete(ep.sigChannels, ep.signingInfo.epoch)
		ep.sigChannelsMu.Unlock()

		if err != nil {
			return err
		}

		// "Collection window closed" — NOT "success". waitForSigs returns
		// (signedWeight, nil) on timeout too (a sub-quorum result is still
		// returned so the decayed voteMajority can be re-checked below), so this
		// log must not imply quorum was reached (audit F7/M-1).
		log.Info("signature collection window closed",
			"block_height", blk,
			"epoch", electionHeader.Epoch,
			"signed_weight", signedWeight)
		ep.signingInfo = nil

		finalCircuit, err := circuit.Finalize()
		if err != nil {
			log.Error("finalize BLS circuit failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
			return err
		}

		bv := finalCircuit.RawBitVector()
		votedWeight := uint64(0)
		totalWeight := uint64(0)
		for i := 0; i < bv.BitLen(); i++ {
			if bv.Bit(i) == 1 {
				votedWeight += electionResult.Weights[i]
			}
			totalWeight += electionResult.Weights[i]
		}

		blocksSinceLastElection := anchorBh
		if !firstElection {
			blocksSinceLastElection = anchorBh - electionResult.BlockHeight
		}
		var activeVersion consensusversion.Version
		if !firstElection {
			activeVersion = elections.ResultVersion(electionResult)
		}
		voteMajority := elections.MinimalRequiredElectionVotes(blocksSinceLastElection, totalWeight, activeVersion)

		if (votedWeight >= voteMajority) || firstElection {
			// Send out Election to Hive

			circuit, err := finalCircuit.Serialize()
			if err != nil {
				log.Error("serialize BLS circuit failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
				return err
			}

			electionResultJson := struct {
				elections.ElectionHeader
				Signature dids.SerializedCircuit `json:"signature"`
			}{
				electionHeader,
				*circuit,
			}

			jsonBytes, err := json.Marshal(electionResultJson)
			if err != nil {
				return err
			}

			op := ep.txCreator.CustomJson([]string{ep.conf.Get().HiveUsername}, []string{}, VSC_ELECTION_TX_ID, string(jsonBytes))

			tx := ep.txCreator.MakeTransaction([]hivego.HiveOperation{op})

			ep.txCreator.PopulateSigningProps(&tx, nil)

			sig, err := ep.txCreator.Sign(tx)
			if err != nil {
				return fmt.Errorf("failed to update account: %w", err)
			}

			tx.AddSig(sig)

			_, err = ep.txCreator.Broadcast(tx)

			if err != nil {
				return fmt.Errorf("failed to update account: %w", err)
			}

			log.Info("election ratified and broadcast",
				"block_height", blk,
				"epoch", electionHeader.Epoch,
				"voted_weight", votedWeight,
				"vote_majority", voteMajority,
				"total_weight", totalWeight,
				"member_count", len(electionData.Members))
		} else {
			// F7/M-1: a sub-quorum election was previously a SILENT fall-through
			// (waitForSigs's timeout returns (signedWeight, nil) and this branch
			// just `return nil`ed with no signal) — a governance halt looked
			// identical to success in the logs. Surface it loudly so operators
			// and alerting can see it. OBSERVABILITY ONLY: this does NOT emit a
			// chain event / on-chain op (that would be a consensus change on the
			// election path) and does NOT alter control flow — the election is
			// simply not broadcast (unchanged), the prior committee persists, and
			// the next slot's leader retries. No state/CID/TSS effect.
			log.Error("ELECTION FAILED: sub-quorum, not broadcasting",
				"block_height", blk,
				"epoch", electionHeader.Epoch,
				"voted_weight", votedWeight,
				"vote_majority", voteMajority,
				"total_weight", totalWeight,
				"member_count", len(electionData.Members),
				"signed_weight", signedWeight)
		}
		return nil
	}
}

type ScoreMap struct {
	Map         map[string]uint64 // blocks signed, per witness
	Denoms      map[string]uint64 // ban denominator per witness: blocks across the epochs it was elected to
	BannedNodes []string
	Members     []string
	Samples     uint64 // total blocks across the window (history gauge / logging only)
}

// epochScores is one windowed election's data, already read from the DB: the
// committee membership for the epoch and the signer account list of every
// block produced under it. Kept as plain data so computeBanScores — the ban
// math — can be unit-tested without mocking elections/vscBlocks.
type epochScores struct {
	members []string   // election.Members accounts (the committee for this epoch)
	blocks  [][]string // Signers of each block produced under this epoch's committee
}

// computeBanScores is the pure core of scoreMap. For each witness it counts the
// blocks it signed (score) and the blocks it *could* have signed — those
// produced across the epochs it was actually elected to (denom). A witness is
// banned when it signed under thresholdPercent% (75 by default) of its own
// opportunity, once that opportunity reaches minSamples blocks.
//
// Scoring against per-witness opportunity rather than the global block total is
// the dilution fix. A witness can only sign blocks in epochs it was on the
// committee, so charging it for epochs it wasn't elected to (the global
// `samples` total) auto-banned anyone present in fewer than ~3 of the last 4
// epochs no matter how faithfully it signed while serving — and, because a
// banned witness is dropped from future committees and then signs nothing,
// turned a single absent/bad epoch into a self-perpetuating lockout.
func computeBanScores(window []epochScores, minSamples uint64, thresholdPercent uint64) ScoreMap {
	samples := uint64(0)
	witnessMap := map[string]bool{}
	scoreMap := map[string]uint64{}
	denomMap := map[string]uint64{}

	for _, epoch := range window {
		blockCount := uint64(len(epoch.blocks))
		for _, signers := range epoch.blocks {
			for _, signer := range signers {
				scoreMap[signer] += 1
			}
		}
		// A witness's denominator is the blocks of the epochs it was a member
		// of — the only blocks it had any opportunity to sign.
		for _, member := range epoch.members {
			witnessMap[member] = true
			denomMap[member] += blockCount
		}
		samples += blockCount
	}

	// review4 HIGH #40: sort before iterating so Members and BannedNodes are
	// deterministic. Map iteration order is randomised by Go; if either slice
	// is ever consumed by code that depends on order (BLS bitset, slicing at
	// quorum cutoff, tie-break), divergent ordering would break consensus
	// across honest nodes.
	members := make([]string, 0, len(witnessMap))
	for member := range witnessMap {
		members = append(members, member)
	}
	sort.Strings(members)

	bannedNodes := []string{}
	for _, member := range members {
		denom := denomMap[member]
		// Hold off until the witness has enough of its own opportunity for the
		// 75% threshold to be meaningful — avoids banning a freshly added or
		// briefly-serving witness on a noisy handful of blocks. (Because
		// denom <= samples, this also subsumes the old global sample gate: no
		// witness can be banned before the chain has minSamples blocks.)
		if denom < minSamples {
			continue
		}
		if scoreMap[member] < denom*thresholdPercent/100 {
			bannedNodes = append(bannedNodes, member)
		}
	}

	return ScoreMap{
		Map:         scoreMap,
		Denoms:      denomMap,
		Members:     members,
		Samples:     samples,
		BannedNodes: bannedNodes,
	}
}

func (ep *electionProposer) scoreMap() (ScoreMap, error) {
	const electionCount = 4

	// #24: must be 0-length slice with cap N, NOT length-N. The old form
	// pre-pended electionCount zero-valued ElectionResults, so the loop below
	// called GetBlocksByElection(0) that many extra times and inflated the
	// sample counts with genesis blocks.
	results := make([]elections.ElectionResult, 0, electionCount)
	election, err := ep.elections.GetElectionByHeight(math.MaxInt64)
	if err != nil {
		return ScoreMap{}, err
	}

	results = append(results, election)

	for i := uint64(1); i < electionCount; i++ {
		// Guard against uint64 underflow on early chains where the latest
		// epoch is < i. Without this, election.Epoch - i wraps to a very
		// large number, GetElection returns nil, and the loop breaks
		// anyway — but only by accident. Be explicit.
		if election.Epoch < i {
			break
		}

		prevElection := ep.elections.GetElection(election.Epoch - i)

		if prevElection == nil {
			break
		}
		results = append(results, *prevElection)

	}

	// Read each windowed epoch's committee + block signers into plain data,
	// then hand off to the pure scorer.
	window := make([]epochScores, 0, len(results))
	for _, result := range results {
		blocks, err := ep.vscBlocks.GetBlocksByElection(result.Epoch)
		if err != nil {
			return ScoreMap{}, err
		}

		es := epochScores{
			members: make([]string, 0, len(result.Members)),
			blocks:  make([][]string, 0, len(blocks)),
		}
		for _, mbr := range result.Members {
			es.members = append(es.members, mbr.Account)
		}
		for _, block := range blocks {
			es.blocks = append(es.blocks, block.Signers)
		}
		window = append(window, es)
	}

	return computeBanScores(window, ep.scoreMapMinSamples, ep.banThresholdPercent), nil
}

// makeElection re-derives the election for a requested epoch on the SIGNER
// path. The anchor is computed deterministically from the on-chain previous
// election (epoch-1), NOT from any peer-supplied block height, so a malicious
// or restarted proposer cannot shift the sample window / settlement record and
// thereby fork the election CID (audit F5 / CP-0b). The ElectionInterval gate
// is subsumed by canonicalElectionAnchor (anchor is always >= prevBh +
// interval); GenerateElectionAtBlock's barrier + far-ahead guard make a signer
// that is behind the anchor abstain rather than sign a divergent CID.
func (ep *electionProposer) makeElection(epoch uint64) (elections.ElectionHeader, elections.ElectionData, error) {
	if epoch == 0 {
		return elections.ElectionHeader{}, elections.ElectionData{}, errors.New("cannot re-derive genesis election by epoch")
	}
	prevElection := ep.elections.GetElection(epoch - 1)
	if prevElection == nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, fmt.Errorf("no previous election for epoch %d", epoch-1)
	}
	anchorBh := canonicalElectionAnchor(prevElection.BlockHeight, ep.sconf.ConsensusParams().ElectionInterval)
	return ep.GenerateElectionAtBlock(anchorBh)
}

func (ep *electionProposer) waitForSigs(ctx context.Context, election *elections.ElectionResult) (uint64, error) {
	if ep.signingInfo == nil {
		return 0, errors.New("no block signing info")
	}

	ep.sigMu.Lock()
	if ep.sigRunning {
		ep.sigMu.Unlock()
		return 0, errors.New("waitForSigs already running")
	}
	ep.sigRunning = true
	ep.sigMu.Unlock()
	defer func() {
		ep.sigMu.Lock()
		ep.sigRunning = false
		ep.sigMu.Unlock()
	}()

	weightTotal := uint64(0)
	for _, weight := range election.Weights {
		weightTotal += weight
	}

	end := make(chan struct{})

	// Capture references before goroutine starts so the goroutine
	// does not need to access ep.signingInfo (which may become nil on timeout).
	ep.sigChannelsMu.Lock()
	sigChan := ep.sigChannels[ep.signingInfo.epoch]
	ep.sigChannelsMu.Unlock()
	circuit := ep.signingInfo.circuit
	block := ep.signingInfo.block
	epoch := ep.signingInfo.epoch
	expectedCid := ep.signingInfo.cid
	weightRequired := weightTotal * 8 / 10

	var err error
	var signedWeight uint64
	go func() {
		signedWeight = 0

		for signedWeight < weightRequired {
			var signResp *signResponse
			select {
			case <-ctx.Done():
				log.Verbose("signature collector exiting",
					"block_height", block,
					"epoch", epoch,
					"reason", ctx.Err(),
					"signed_weight", signedWeight,
					"weight_required", weightRequired)
				end <- struct{}{}
				return
			case signResp = <-sigChan:
			}

			if signResp == nil {
				log.Warn("signature channel closed before quorum",
					"block_height", block,
					"epoch", epoch,
					"signed_weight", signedWeight,
					"weight_required", weightRequired)
				break
			}

			sigBytes, err := base64.URLEncoding.DecodeString(signResp.Sig)

			if err != nil {
				continue
			}

			sig := blsu.Signature{}
			sig96 := [96]byte{}
			copy(sig96[:], sigBytes[:])
			err = sig.Deserialize(&sig96)

			if err != nil {
				continue
			}

			sigStr := signResp.Sig
			account := signResp.Account

			var member dids.Member
			var index int
			for i, data := range election.Members {
				if data.Account == account {
					member = dids.BlsDID(data.Key)
					index = i
					break
				}
			}

			c := *circuit

			added, err := c.AddAndVerify(member, sigStr)

			if err != nil {
				log.Warn("signature aggregate failed",
					"block_height", block,
					"epoch", epoch,
					"account", account,
					"err", err)
			} else if added {
				signedWeight += election.Weights[index]
				log.Verbose("signature aggregated",
					"block_height", block,
					"epoch", epoch,
					"account", account,
					"added_weight", election.Weights[index],
					"signed_weight", signedWeight,
					"weight_required", weightRequired)
			} else {
				log.Warn("signature ignored",
					"block_height", block,
					"epoch", epoch,
					"account", account,
					"expected_cid", expectedCid,
					"expected_msg_hex", hex.EncodeToString(c.Msg().Bytes()),
					"received_sig", sigStr,
					"reason", "already aggregated or did not verify")
			}
		}
		log.Verbose("signature collector reached quorum",
			"block_height", block,
			"epoch", epoch,
			"signed_weight", signedWeight,
			"weight_required", weightRequired)
		end <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		log.Warn("signature collection timeout",
			"block_height", block,
			"epoch", epoch,
			"reason", ctx.Err(),
			"signed_weight", signedWeight,
			"weight_required", weightRequired)
		<-end // Wait for goroutine to finish before returning
		return signedWeight, nil
	case <-end:
		return signedWeight, err
	}
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}

func resultJoin[T any](results ...result.Result[T]) (res result.Result[[]T]) {
	for _, r := range results {
		if r.IsOk() {
			res = result.Ok(append(res.Unwrap(), r.Unwrap()))
		} else {
			return result.Map(r, func(T) []T {
				return nil
			})
		}
	}
	return res
}

// poaActive reports whether the POA election rules apply: the batch is active AND
// this node actually has the seat registry wired.
//
// ★ THE REGISTRY CHECK IS NOT DEFENSIVE BOILERPLATE. Flat seat-weight without
// the seat gate is strictly WORSE than either regime it sits between: candidacy
// reverts to "anyone with MinStake", but every such candidate then carries the
// same weight as a vetted operator — so an attacker buys committee weight at
// MinStake per seat instead of proportionally, which is cheaper than the
// stake-weighting POA replaced AND free of the vetting POA adds. Gating every
// POA election rule on the same condition means the rules can only ever apply as
// a set: either this node has a registry and enforces seats and flat weight
// together, or it applies neither and behaves exactly as it does today.
func (e *electionProposer) poaActive(prevVersion consensusversion.Version) bool {
	return consensusversion.PoaFlatWeightActive(prevVersion) && e.poaSeats != nil
}
