package params

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"vsc-node/modules/common/consensusversion"
)

// A transaction consuming 1000 RC (1 HBD equivalent) would generate ~0.002 HBD interest for the protocol
// At 100K gas/RC and 100 RC minimum cost it would take at least 10M gas for a tx to consume more
const CYCLE_GAS_PER_RC = 100_000

// Areweave does $10.5 per GB we can use less b/c we charge for reads and modifications as well
// 19 RCs per new written byte ($4/GB)
// 1 RC per read or modified byte ($0.21/GB)
const WRITE_IO_GAS_RC_COST = 19
const READ_IO_GAS_RC_COST = 1

const EPHEM_IO_GAS = 100

// 2,000 HIVE
var CONSENSUS_MINIMUM = int64(2_000_000)

var MAINNET_ID = "vsc-mainnet"

var GATEWAY_WALLET = "vsc.gateway"

var FR_VIRTUAL_ACCOUNT = "system:fr_balance"

var DAO_WALLET = "hive:vsc.dao"

// ProtocolSlashBurnAccount is the ledger owner for safety-slash amounts that are
// not paid as restitution. Rows are audit-only: they must not increase spendable
// HIVE (see state engine balance aggregation and GetBalance op-type rules).
//
// HISTORICAL: matured slash residual USED to land here (destroyed). It now lands
// on ProtocolSlashReserveAccount instead (the slash-reserve workstream). This
// account is retained only so any pre-existing burn rows keep resolving; no new
// rows are written to it (safety slashing has never fired on any network — no
// activation height has been pinned, see SafetySlashActivationHeight — so there
// are none in practice).
var ProtocolSlashBurnAccount = "system:protocol_slash_burn"

// ProtocolSlashReserveAccount is the keyless insurance reserve. This is a
// DESTINATION CHANGE ONLY: the safety-slash residual (the part not paid as
// restitution at slash time) lands here INSTEAD of being burned. No key, no
// multisig, no owner — only state-engine rules ever write it, and there is NO
// extraction path: value never leaves this account. It is a permanent retained
// INSURANCE backstop, recoverable in principle only by a future,
// separately-designed make-whole mechanism (none exists or ships here). It is
// NOT part of the V/E collateral computation (modules/incentive-pendulum
// collateral_int.go): collateral is the HIVE_CONSENSUS bond, and nothing reads
// this reserve into E, V, or s. Like THORChain's Reserve (which is a retained
// treasury, not collateral — the bond is collateral there too), this is a held
// insurance pool, not over-collateralization.
//
// Rows MUST NOT increase spendable HIVE: the op-type LedgerTypeSafetySlashReserve
// is excluded from BOTH balance-aggregation sites (the GetBalance allow-list AND
// the UpdateBalances snapshot deny-list switch), exactly like the burn account it
// replaces. The ONLY observable difference from burning is that the value is
// retained (recoverable later) rather than destroyed.
//
// WHY no make-whole here: the only live slash triggers are equivocation
// (double-block-sign) and invalid-block-proposal — consensus-integrity faults
// with NO fund victim (orphaned txs re-execute; rejected-block txs never
// executed). Both THORChain and Polkadot route non-theft slashes to a
// Reserve/treasury with no victim payout; only THEFT (a real vault outflow) has
// a victim+amount to make whole, and VSC has no theft detector. An earlier draft
// added a victim-payout extraction here; a full audit proved its harm-proof was
// structurally dead (AnchoredId can never equal the slash tx id for a slashed
// block) and that the faults have no victims, so it was removed. Reserve =
// backstop only, until a real theft detector is built as a separate workstream.
var ProtocolSlashReserveAccount = "system:protocol_slash_reserve"

// ProtocolSlashPendingBurnAccount holds the liquid HIVE residual of a safety
// slash during the challenge window (until BurnDelayBlocks passes), then
// FinalizeMaturedSafetySlashBurns promotes it to ProtocolSlashReserveAccount
// (destination change — no longer burned). Not spendable (excluded from balance
// aggregation). Name kept ("...BurnPending") for compatibility.
var ProtocolSlashPendingBurnAccount = "system:protocol_slash_burn_pending"

// ProtocolSlashFinalizeCursorAccount stores the single-row scan cursor used by
// FinalizeMaturedSafetySlashBurns. The cursor's value (in the From field) is
// the lowest emission BlockHeight at which any pending burn row could still be
// unfinalized. On every UpdateBalances tick, the finalizer scans only
// [cursor, blockHeight] instead of [0, blockHeight], so the cost is bounded by
// the active pending-row window rather than chain length. Account is balance-
// neutral (every row has Amount=0); it exists purely as a meta marker.
var ProtocolSlashFinalizeCursorAccount = "system:protocol_slash_finalize_cursor"

// MaxSafetySlashBurnDelayBlocks caps BurnDelayBlocks to avoid uint64 maturity
// overflow and unbounded pending queues. ~115 days at 3s/block.
const MaxSafetySlashBurnDelayBlocks uint64 = 3_333_333

var RC_RETURN_PERIOD uint64 = 120 * 60 * 20 // 5 day cool down period for RCs
var RC_HIVE_FREE_AMOUNT int64 = 10_000      // 5 HBD worth of RCs for Hive accounts
var MINIMUM_RC_LIMIT uint64 = 50

var CONTRACT_DEPLOYMENT_FEE int64 = 10_000 // 10 HBD per contract
var CONTRACT_DEPLOYMENT_FEE_START_HEIGHT uint64 = 99410000
var CONTRACT_UPDATE_HEIGHT uint64 = 102100000

// PENDULUM_FEE_FIX_HEIGHT is the mainnet activation height (Hive L1 block) for
// the 2026-06 pendulum swap-fee overcharge fix (CLP-leg scaling + total-fee
// clamp in incentive-pendulum/wasm). Below this height the applier reproduces
// the pre-fix fee math bit-for-bit so witnesses can upgrade across the rollout
// window without diverging; at/after it every witness switches to the bounded
// fee atomically. ~6h after the 2026-06-18 deploy decision (head ≈107,389,200 +
// 7200 blocks @3s; cf. ELECTION_INTERVAL = 6*60*20). Mainnet only — testnet and
// devnet run the fix immediately (Config.ActivationHeight 0).
var PENDULUM_FEE_FIX_HEIGHT uint64 = 107_396_400
var CONTRACT_CALL_MAX_RECURSION_DEPTH = 20

// ───── Contract update timelock ─────
//
// A vsc.update_contract is queued and only takes effect after this many Hive L1
// blocks (3s/block) have passed since it was submitted. During the window the
// previously-active code keeps running; the active version at any height H is
// the newest one whose activation_height <= H (see contracts.ContractById).
//
// This is a CONSENSUS RULE enforced on the deterministic state-processing replay
// path, so it MUST be identical on every node. The value is therefore network-
// baked in system-config and intentionally NOT exposed via -sysconfig overrides:
// no single operator can shorten it (a custom binary just forks that node off
// consensus, since the honest supermajority won't sign its divergent state).

// CONTRACT_UPDATE_TIMELOCK_BLOCKS is the mainnet timelock: 57,600 blocks ≈ 48h.
var CONTRACT_UPDATE_TIMELOCK_BLOCKS uint64 = 57_600

// CONTRACT_UPDATE_TIMELOCK_BLOCKS_TESTNET is the shorter delay used on test
// networks (testnet/devnet) so the timelock is exercisable: 30 blocks ≈ 90s.
// Mocknet uses 0 (disabled) so the in-process e2e harness keeps its immediate-
// update mechanics.
//
// The timelock's ACTIVATION (vs. its duration here) is gated on the chain-active
// consensus version reaching 0.2.0 (consensusversion.ContractUpdateTimelockActive),
// driven per-network by the ConsensusVersionFloor* in system-config — not a
// dedicated height.
var CONTRACT_UPDATE_TIMELOCK_BLOCKS_TESTNET uint64 = 30

// review2 LOW #70/#110: contract-call payloads were only UTF-8 checked,
// with no explicit length cap — the node implicitly relied on Hive's
// ~8KB custom_json limit. Cap explicitly so the bound is enforced
// deterministically by the node itself, independent of the L1 path.
var MAX_CONTRACT_PAYLOAD_SIZE = 8 * 1024 // bytes

// Maximum age (in Hive L1 blocks) for a TSS commitment's self-declared
// BlockHeight relative to the carrying transaction's block. Commitments
// older than this are rejected to prevent replay of stale aggregate BLS
// signatures against long-retired elections. 28800 blocks ≈ 24 hours at
// 3s/block, matching tss.BLAME_EXPIRE.
var TSS_COMMITMENT_MAX_STALENESS = uint64(28800)

// Mainnet TSS key indexing
var TSS_INDEX_HEIGHT uint64 = 102_083_000

// Election once every 6 hours on mainnet
var ELECTION_INTERVAL = uint64(6 * 60 * 20)

type ConsensusParams struct {
	MinStake             int64  `json:"minStake,omitempty"`
	MinMembers           int    `json:"minMembers,omitempty"`
	MinSpSigners         int    `json:"minSpSigners,omitempty"`
	MinRcLimit           uint64 `json:"minRcLimit,omitempty"`
	TssIndexHeight       uint64 `json:"tssIndexHeight,omitempty"`
	ElectionInterval     uint64 `json:"electionInterval,omitempty"`
	ElectionDupeFixEpoch uint64 `json:"electionDupeFixEpoch,omitempty"`

	// PendulumSeedEpoch bootstraps the pendulum settlement chain on a network
	// that pre-dates inlined settlement. On startup, if the pendulum_settlements
	// collection is empty, the state engine seeds a single marker for this epoch
	// so GetLatestSettledEpoch() returns a value every node agrees on — without
	// it, an in-place upgrade leaves latestSettledEpoch at 0 and the proposer's
	// canHold gate deadlocks. Must be a fixed network-wide constant (same on
	// every node, reindex or not). 0 means "no seed" — correct for fresh chains
	// built from genesis with the settlement code already present.
	PendulumSeedEpoch uint64 `json:"pendulumSeedEpoch,omitempty"`

	// EvmAddressChecksumHeight gates EIP-55 checksum normalization of EVM
	// destination addresses on gateway deposits. A deposit at
	// BlockHeight >= this value has its resolved did:pkh:eip155 owner
	// normalized to the canonical EIP-55 (mixed-case) checksum form, so
	// that the same Ethereum address can no longer fragment a balance
	// across multiple case-variant DIDs. Below this height the legacy
	// behavior is preserved: the owner keeps the exact casing from the
	// deposit memo.
	//
	// MUST be a fixed network-wide constant (identical on every node,
	// reindex or not) set to a height STRICTLY ABOVE the current chain
	// head before rollout. A height at or below an already-processed block
	// is a consensus footgun: live nodes credited those historical
	// deposits verbatim, but a fresh reindex would credit them normalized,
	// so the two would diverge. 0 disables normalization entirely (legacy
	// verbatim behavior) — the correct default until an operator pins the
	// activation height for a deploy.
	EvmAddressChecksumHeight uint64 `json:"evmAddressChecksumHeight,omitempty"`

	// NOTE: the v0.2.0 release batch is no longer gated by a dedicated activation
	// HEIGHT. Every consensus-affecting change shipping in v0.2.0 now gates on the
	// CHAIN-ACTIVE CONSENSUS VERSION reaching 0.2.0 (consensusversion.V0_2_0 and
	// the WitnessKeyStrictActive / ContractUpdateTimelockActive /
	// GatewayDecentralizationActive resolvers in
	// modules/common/consensusversion/feature_gates.go), the same mechanism
	// try/catch and the pendulum LP-floor already use. Per-network rollout is
	// therefore driven by the consensus-version floor below
	// (ConsensusVersionFloor{Epoch,Major,Consensus} / a vsc.propose_consensus_version),
	// which only rises once a stake-supermajority is attested running 0.2.0 — so
	// the old "pin a height strictly above head or fork across the gap" footgun is
	// gone. To roll out v0.2.0: raise ConsensusVersionFloorConsensus to 2 at a
	// future epoch boundary (mainnet); ephemeral nets pin the floor low.

	// RecoveryMultisigAccounts are Hive account names (no hive: prefix) authorized to post
	// vsc.recovery_suspend and vsc.recovery_require_version.
	RecoveryMultisigAccounts []string `json:"recoveryMultisigAccounts,omitempty"`
	// RecoveryMultisigThreshold is M in M-of-N (distinct accounts from RecoveryMultisigAccounts in required_auths).
	RecoveryMultisigThreshold int `json:"recoveryMultisigThreshold,omitempty"`
	// ConsensusVersionActivationNum/Den is the stake fraction (Num/Den) of the current committee
	// that must already announce a scheduled target version before it activates at its epoch.
	// Defaults to 4/5 (80%) when unset.
	ConsensusVersionActivationNum uint64 `json:"consensusVersionActivationNum,omitempty"`
	ConsensusVersionActivationDen uint64 `json:"consensusVersionActivationDen,omitempty"`

	// ConsensusVersionFloorEpoch / ...Major / ...Consensus pin a deterministic consensus
	// version floor WITHOUT an on-chain proposal or stake-readiness guard (the "simple
	// rollout" path). At/after ConsensusVersionFloorEpoch, every election's version floor
	// rises to {ConsensusVersionFloorMajor, ConsensusVersionFloorConsensus}, so any witness
	// announcing a lower major.consensus is excluded from the committee (and, downstream,
	// from TSS gating). Use this when the operator knows out-of-band that the supermajority
	// will be upgraded by the activation epoch, so the propose/activation path's stake
	// readiness guard is unnecessary.
	//
	// MUST be a fixed network-wide constant (identical on every node, reindex or not). The
	// target (Major/Consensus) is read from config — NOT from the node's own RunningVersion —
	// so the floor is a pure function of config + newEpoch and does not depend on which version
	// the node itself runs; every node resolves the identical floor (deriving it from
	// RunningVersion instead could diverge across a mixed-version fleet). A witness whose
	// announced version is below the resolved floor is simply excluded. Epoch 0 disables the pin
	// (the floor then moves only via the propose/recovery paths). Set the epoch to the first
	// post-rollout election epoch (typically PendulumSeedEpoch + 1).
	ConsensusVersionFloorEpoch     uint64 `json:"consensusVersionFloorEpoch,omitempty"`
	ConsensusVersionFloorMajor     uint64 `json:"consensusVersionFloorMajor,omitempty"`
	ConsensusVersionFloorConsensus uint64 `json:"consensusVersionFloorConsensus,omitempty"`

	// BondInclusionWindowBlocks (W) is the bond inclusion window: a witness's
	// HIVE_CONSENSUS counts toward the election only once held continuously for
	// W blocks (mainnet 86,400 ≈ 3 days). See modules/election-proposer/
	// bond_maturity.go. W==0 means the maturity gate is disabled (point-in-time,
	// legacy behavior) even if the activation height below is reached.
	BondInclusionWindowBlocks uint64 `json:"bondInclusionWindowBlocks,omitempty"`
	// BondInclusionActivationHeight gates the window: elections at
	// blockHeight >= this value apply the maturity gate; below it — and when 0 —
	// the legacy raw HIVE_CONSENSUS read is used (so 0 = inert / no behavior
	// change, the safe default until an operator pins a deploy height). Kept as
	// its OWN height (NOT folded into the v0.2.0 version line) because the window
	// must activate only after a committee has formed — see BondInclusionActive. MUST
	// be a fixed network-wide constant set STRICTLY ABOVE the current head, on an
	// epoch boundary, with >=3d lead, identical on every node (reindex or not):
	// a height at/below an already-processed block diverges live vs reindex —
	// same consensus footgun as EvmAddressChecksumHeight.
	BondInclusionActivationHeight uint64 `json:"bondInclusionActivationHeight,omitempty"`
	// BondInclusionSampleCount is the number of min-over-window samples used to
	// compute matured stake. Values < 2 are floored to 2 by the gate so a
	// mis-set/zero value can never silently collapse the window to a
	// point-in-time read.
	BondInclusionSampleCount int `json:"bondInclusionSampleCount,omitempty"`

	// MaxNewMembersPerElection (audit F6 / THORChain NumberOfNewNodesPerChurn)
	// caps how many NEW members (accounts not in the previous ratified election)
	// the bond inclusion gate admits in a single election. The maturity window
	// staggers INDIVIDUAL stakers, but a coordinated cohort that all matures on
	// the same boundary would otherwise enter atomically in ONE election — a
	// majority flip with no per-epoch reaction window. The cap admits only the
	// highest-effective-stake subset of new entrants per election; the rest
	// wait for subsequent elections, so a hostile cohort enters GRADUATED (a
	// few seats per epoch) instead of all at once. 0 == disabled (no cap — the
	// safe inert default; only meaningful when the bond gate is active, and
	// only consulted then). It does NOT cap incumbents or floor-guard
	// backfills (those are not "new"), so it can never fight the floor guard.
	MaxNewMembersPerElection int `json:"maxNewMembersPerElection,omitempty"`

	// BondInclusionEstablishedGraceBlocks is the established-member exception to
	// the inclusion window (operator requirement). A witness that has served as
	// a RATIFIED committee member in at least one past election ("established")
	// does NOT have to re-pass the 3-day window for the stake it was already
	// ratified for — even if it drops out — for up to this many blocks of
	// absence (2 weeks @ 3s = 403,200). The exempt amount is capped at
	// min(current balance, last-ratified weight), so it can ONLY restore
	// already-earned, already-ratified power — NEW stake (top-ups, or anyone's
	// fresh deposits) still goes through the window, and an account-sale cannot
	// fast-track new stake. The exception is purely ADDITIVE (effective stake =
	// max(matured, grandfather, established)); it can never lower a member's
	// stake or evict an established member who still holds their bond. Gone for
	// LONGER than this grace ⇒ the exception expires and the member must
	// re-mature. 0 == disabled. Only consulted when the bond gate is active.
	BondInclusionEstablishedGraceBlocks uint64 `json:"bondInclusionEstablishedGraceBlocks,omitempty"`

	// SafetySlashWindows is the schedule of half-open [Start, End) block-height
	// ranges over which verifiable-fault PRINCIPAL slashing (the HIVE_CONSENSUS
	// bond debit in slashForEvidenceIfPolicyAllows → SafetySlashConsensusBond) is
	// IN FORCE. Outside every window the detectors still run and log but never
	// touch the ledger. Semantics (see SafetySlashActive):
	//   - empty / nil → slashing is never active (inert; the safe default).
	//   - End == 0    → open-ended: active from Start onward. Only the FINAL
	//                   window may be open-ended.
	//   - End != 0    → active for Start <= h < End (End exclusive).
	// An on→off→on cycle is expressed by closing the current window (set its End)
	// and APPENDING a new one — never by editing an existing entry. Kept as its OWN
	// per-network schedule (NOT folded into the v0.2.0 version line) because
	// principal slashing is on a more cautious maturity timeline than the rest of
	// v0.2.0 and must be stageable independently.
	//
	// CONSENSUS-CRITICAL: the slash changes ledger state, so every boundary MUST be
	// a fixed network-wide constant, identical on every node (reindex or not). Any
	// boundary at or below current chain head is FROZEN history — live nodes already
	// ran each past block's slash/no-slash path, so a reindex on a binary that moved
	// a past boundary would debit (or skip) different bonds and diverge, exactly
	// like EvmAddressChecksumHeight. Only ever APPEND a window, or set the End of the
	// final still-future window, STRICTLY ABOVE chain head, with every witness
	// upgraded BEFORE Hive reaches it. A malformed schedule (overlapping, out of
	// order, or mid-list open-ended) is rejected by ValidSafetySlashWindows, which
	// is asserted against every shipped config in tests.
	SafetySlashWindows []HeightWindow `json:"safetySlashWindows,omitempty"`

	// GovernanceProposalExpiryBlocks is how long a witness-vote governance
	// proposal (vsc.slash_restore / vsc.reserve_payout) stays open to collect
	// approvals, in L1 blocks. A proposal that has not crossed the 2/3 threshold
	// by creationBlock+this is closed. For a slash_restore the effective window
	// is additionally capped to the slash's remaining pending time
	// (min(expiry, maturity−creation) — see governance.EffectiveExpiry); it never
	// extends the slash's fixed maturity. CONSENSUS-CRITICAL: the tally/expiry
	// decision changes ledger state, so this is a fixed network-wide constant
	// every node shares. 0 falls back to DefaultGovernanceProposalExpiryBlocks.
	GovernanceProposalExpiryBlocks uint64 `json:"governanceProposalExpiryBlocks,omitempty"`

	// SafetySlashBurnDelay7dHeight is the FORWARD-ONLY activation height for the
	// extended 7-day safety-slash pending window (vs the original 3 days). A slash
	// occurring at slashHeight uses the 7-day window iff slashHeight >= this value
	// (and this != 0); earlier slashes keep the 3-day window. The longer window
	// gives witnesses more time to notice a wrongful slash and gather a
	// slash_restore quorum before the residual matures into the reserve.
	//
	// CONSENSUS-CRITICAL: the window length is read at slash-application time and
	// folded into the STORED maturity (slashHeight + delay). Because the gate keys
	// off the slash's OWN height it is recomputed identically on replay, so raising
	// the window can NEVER retroactively shift a historical maturity (which would
	// diverge a resync — the same footgun as SafetySlashWindows). Pin it STRICTLY
	// ABOVE chain head with every witness upgraded first. 0 = not yet scheduled
	// (stays 3 days), the safe default.
	SafetySlashBurnDelay7dHeight uint64 `json:"safetySlashBurnDelay7dHeight,omitempty"`
}

// DefaultGovernanceProposalExpiryBlocks is the fallback proposal voting window
// (~3 days at 3s/block) used when a network pins no explicit value.
const DefaultGovernanceProposalExpiryBlocks uint64 = 3 * 28800

const (
	// SafetySlashBurnDelay3dBlocks is the original ~3-day pending-burn window
	// (mirrors safetyslash.DefaultSafetySlashBurnDelayBlocks; kept here to avoid a
	// params→safety_slash import just for the gate).
	SafetySlashBurnDelay3dBlocks uint64 = 3 * 28800
	// SafetySlashBurnDelay7dBlocks is the extended ~7-day pending-burn window.
	SafetySlashBurnDelay7dBlocks uint64 = 7 * 28800
)

// SafetySlashBurnDelayBlocks returns the pending-burn challenge-window length (in
// L1 blocks) for a slash occurring at slashHeight: the 7-day window at/after
// SafetySlashBurnDelay7dHeight, the 3-day window before it. Keying off the slash's
// own height keeps maturity deterministic across replay (see the field comment).
func (cp ConsensusParams) SafetySlashBurnDelayBlocks(slashHeight uint64) uint64 {
	if cp.SafetySlashBurnDelay7dHeight != 0 && slashHeight >= cp.SafetySlashBurnDelay7dHeight {
		return SafetySlashBurnDelay7dBlocks
	}
	return SafetySlashBurnDelay3dBlocks
}

// HeightWindow is a half-open [Start, End) range of L1 block heights. End == 0
// means open-ended (no upper bound). Used by SafetySlashWindows to schedule
// on/off cycles of a consensus rule; see ValidSafetySlashWindows for the
// well-formedness invariant a schedule of these must satisfy.
type HeightWindow struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end,omitempty"`
}

// ───── Named activation predicates (Ethereum ChainConfig style) ─────
//
// Each height-gated rule on ConsensusParams gets a single named predicate so the
// "is feature X active at this height" question lives in one place — including the
// `0 == disabled` convention where it applies. Use these at the call site instead
// of bare numeric comparisons, so a future tweak to a gate's semantics (e.g. `>`
// vs `>=`, or pulling in a network check) is a one-line edit, not a grep.

// EvmAddressChecksumActive returns true at blockHeight when EIP-55 normalization
// of EVM destination addresses on gateway deposits should be applied. Zero
// height means the feature is disabled entirely (legacy verbatim casing).
func (cp ConsensusParams) EvmAddressChecksumActive(blockHeight uint64) bool {
	return cp.EvmAddressChecksumHeight != 0 && blockHeight >= cp.EvmAddressChecksumHeight
}

// NOTE: the v0.2.0 batch resolvers (Version0_2_0Active / WitnessKeyStrictActive /
// ContractUpdateTimelockActive / GatewayDecentralizationActive) moved to
// modules/common/consensusversion/feature_gates.go and now take the chain-active
// consensus version (resolved from the on-chain election) instead of a block
// height — see V0_2_0 there. SafetySlashActive and BondInclusionActive keep their
// OWN heights below because they must be stageable independently of the v0.2.0
// version line (see each predicate's note).

// SafetySlashActive reports whether verifiable-fault principal (HIVE_CONSENSUS
// bond) slashing is in force at blockHeight — true iff blockHeight falls within
// one of the configured SafetySlashWindows. An empty schedule is never active; a
// final window with End == 0 is active from its Start onward. Call sites gate the
// ledger debit on this so the decision is a pure function of (schedule, height)
// and replays identically on every node. Assumes a well-formed schedule
// (ValidSafetySlashWindows); the lookup itself tolerates any slice but a
// malformed schedule's meaning is only defined once it passes validation.
func (cp ConsensusParams) SafetySlashActive(blockHeight uint64) bool {
	for _, w := range cp.SafetySlashWindows {
		if blockHeight >= w.Start && (w.End == 0 || blockHeight < w.End) {
			return true
		}
	}
	return false
}

// ValidSafetySlashWindows reports an error if the SafetySlashWindows schedule is
// malformed. Well-formed means a sorted list of disjoint half-open [Start, End)
// intervals in which every window is non-empty (Start < End) and only the FINAL
// window may be open-ended (End == 0). An empty schedule is valid (slashing never
// active). Because the slash debit is replay-derived, a malformed schedule would
// diverge nodes, so this is asserted against every shipped network config in
// tests — a bad constant fails the build rather than forking the chain at runtime.
func (cp ConsensusParams) ValidSafetySlashWindows() error {
	ws := cp.SafetySlashWindows
	for i, w := range ws {
		last := i == len(ws)-1
		if w.End == 0 {
			if !last {
				return fmt.Errorf("safety slash window %d {start:%d}: only the final window may be open-ended (End == 0)", i, w.Start)
			}
			continue // open-ended final window: nothing after it to order against
		}
		if w.End <= w.Start {
			return fmt.Errorf("safety slash window %d: End (%d) must be greater than Start (%d)", i, w.End, w.Start)
		}
		if !last && w.End > ws[i+1].Start {
			return fmt.Errorf("safety slash windows %d and %d overlap or are out of order: End %d > next Start %d", i, i+1, w.End, ws[i+1].Start)
		}
	}
	return nil
}

// GovernanceProposalExpiry returns the configured proposal voting window in L1
// blocks, falling back to DefaultGovernanceProposalExpiryBlocks when unset (0).
func (cp ConsensusParams) GovernanceProposalExpiry() uint64 {
	if cp.GovernanceProposalExpiryBlocks == 0 {
		return DefaultGovernanceProposalExpiryBlocks
	}
	return cp.GovernanceProposalExpiryBlocks
}

// BondInclusionActive reports whether the bond inclusion-window maturity gate
// applies to an election generated at blockHeight. Unlike the version-gated
// v0.2.0 batch, the bond window keeps its OWN activation height: it must activate
// only AFTER a committee has formed and witnesses have a full window of stake
// history — activating it the instant the floor reaches 0.2.0 (which ephemeral
// nets pin low) would leave fresh stake unmatured. So activationHeight==0 disables
// it entirely (legacy raw HIVE_CONSENSUS read); otherwise it applies at
// blockHeight >= activationHeight. NOTE: the window length W
// (BondInclusionWindowBlocks) being 0 also disables the maturity requirement even
// when active — handled inside maturedConsensusStake — so callers gate on this
// predicate AND pass W through.
func (cp ConsensusParams) BondInclusionActive(blockHeight uint64) bool {
	return cp.BondInclusionActivationHeight != 0 && blockHeight >= cp.BondInclusionActivationHeight
}

// TssIndexed returns true at blockHeight when TSS state should be indexed for this
// network. Callers that need network-aware gating (e.g. "only gate on testnet")
// should keep their explicit OnTestnet/OnMainnet check around this predicate.
func (cp ConsensusParams) TssIndexed(blockHeight uint64) bool {
	return blockHeight >= cp.TssIndexHeight
}

// ElectionDupeFixActive returns true at epoch when the election-result dedup
// fix is in force. Below this epoch the legacy behavior (no dedup) applies.
func (cp ConsensusParams) ElectionDupeFixActive(epoch uint64) bool {
	return epoch >= cp.ElectionDupeFixEpoch
}

// PendulumSeedConfigured returns true when this network has a pendulum
// settlement bootstrap epoch configured (i.e. PendulumSeedEpoch != 0). Used to
// gate the one-time bootstrap marker write on chains that pre-date settlement.
func (cp ConsensusParams) PendulumSeedConfigured() bool {
	return cp.PendulumSeedEpoch != 0
}

// PinnedVersionFloor returns the config-pinned consensus floor that applies to an election
// creating newEpoch, or the zero version when no pin applies (ConsensusVersionFloorEpoch is
// 0, or newEpoch precedes it). Deterministic: a pure function of network config and newEpoch
// that does not depend on the node's own running version, so every node resolves the identical
// floor. The election proposer raises its floor to this value and never lowers an already-higher
// floor.
func (cp ConsensusParams) PinnedVersionFloor(newEpoch uint64) consensusversion.Version {
	if cp.ConsensusVersionFloorEpoch == 0 || newEpoch < cp.ConsensusVersionFloorEpoch {
		return consensusversion.Version{}
	}
	return consensusversion.Version{
		Major:     cp.ConsensusVersionFloorMajor,
		Consensus: cp.ConsensusVersionFloorConsensus,
	}
}

type TssParams struct {
	ReshareSyncDelay      time.Duration `json:"reshareSyncDelay,omitempty"`
	ReshareTimeout        time.Duration `json:"reshareTimeout,omitempty"`
	DefaultTimeout        time.Duration `json:"defaultTimeout,omitempty"`
	MessageRetryDelay     time.Duration `json:"messageRetryDelay,omitempty"`
	BufferedMessageMaxAge time.Duration `json:"bufferedMessageMaxAge,omitempty"`
	RpcTimeout            time.Duration `json:"rpcTimeout,omitempty"`
	CommitDelay           time.Duration `json:"commitDelay,omitempty"`
	WaitForSigsTimeout    time.Duration `json:"waitForSigsTimeout,omitempty"`
	RotateInterval        uint64        `json:"rotateInterval,omitempty"`
	SignInterval          uint64        `json:"signInterval,omitempty"`
	ReadinessOffset       uint64        `json:"readinessOffset,omitempty"`
	// PreParamsTimeout is the maximum time to spend generating Paillier
	// key pairs and safe primes for TSS. Defaults to 1 minute if zero.
	// Set higher (e.g. 10m) in test/CI environments where multiple nodes
	// compete for CPU and prime generation takes longer.
	PreParamsTimeout time.Duration `json:"preParamsTimeout,omitempty"`
}

// MarshalJSON serializes TssParams with durations as human-readable
// strings (e.g. "5s", "2m") and uint64 fields as numbers.
func (t TssParams) MarshalJSON() ([]byte, error) {
	m := make(map[string]interface{})
	v := reflect.ValueOf(t)
	rt := v.Type()
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		key, _, _ := strings.Cut(tag, ",")
		field := v.Field(i)
		switch field.Kind() {
		case reflect.Int64: // time.Duration
			dur := field.Interface().(time.Duration)
			if dur != 0 {
				m[key] = dur.String()
			}
		case reflect.Uint64:
			val := field.Uint()
			if val != 0 {
				m[key] = val
			}
		}
	}
	return json.Marshal(m)
}

// UnmarshalJSON deserializes TssParams from a JSON object where
// durations are human-readable strings and intervals are numbers.
// Only fields present in the JSON are overwritten.
func (t *TssParams) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	v := reflect.ValueOf(t).Elem()
	rt := v.Type()
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		key, _, _ := strings.Cut(tag, ",")
		raw, ok := m[key]
		if !ok {
			continue
		}
		field := v.Field(i)
		switch field.Kind() {
		case reflect.Int64: // time.Duration
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("field %s: expected duration string: %w", key, err)
			}
			d, err := time.ParseDuration(s)
			if err != nil {
				return fmt.Errorf("field %s: %w", key, err)
			}
			field.Set(reflect.ValueOf(d))
		case reflect.Uint64:
			var n uint64
			if err := json.Unmarshal(raw, &n); err != nil {
				return fmt.Errorf("field %s: expected number: %w", key, err)
			}
			field.SetUint(n)
		}
	}
	return nil
}

var DefaultTssParams = TssParams{
	ReshareSyncDelay:      5 * time.Second,
	ReshareTimeout:        2 * time.Minute,
	DefaultTimeout:        1 * time.Minute,
	MessageRetryDelay:     1 * time.Second,
	BufferedMessageMaxAge: 1 * time.Minute,
	RpcTimeout:            30 * time.Second,
	CommitDelay:           5 * time.Second,
	WaitForSigsTimeout:    6 * time.Second,
}

var MocknetTssParams = TssParams{
	ReshareSyncDelay:      1 * time.Second,
	ReshareTimeout:        2 * time.Minute,
	DefaultTimeout:        1 * time.Minute,
	MessageRetryDelay:     500 * time.Millisecond,
	BufferedMessageMaxAge: 30 * time.Second,
	RpcTimeout:            10 * time.Second,
	CommitDelay:           1 * time.Second,
	WaitForSigsTimeout:    6 * time.Second,
}

type OracleParams struct {
	// ChainContracts maps chain symbols (e.g. "BTC") to their
	// relay mapping contract IDs.
	ChainContracts map[string]string `json:"chainContracts,omitempty"`

	// ZKVerifierChains lists chain symbols whose headers are provided by
	// a ZK prover submitting directly to a verifier contract. When a chain
	// is in this map, the oracle skips BLS relay for it.
	ZKVerifierChains map[string]string `json:"zkVerifierChains,omitempty"`

	// Deprecated: use ChainContracts["BTC"] instead.
	BtcContractId string `json:"btcContractId,omitempty"`
}

// HasZKVerifier returns true if the given chain uses ZK proof verification
// instead of oracle BLS relay.
func (o OracleParams) HasZKVerifier(symbol string) bool {
	if o.ZKVerifierChains == nil {
		return false
	}
	_, ok := o.ZKVerifierChains[symbol]
	return ok
}

// ContractId returns the relay contract ID for the given chain symbol.
// Falls back to the legacy BtcContractId field for BTC.
func (o OracleParams) ContractId(symbol string) string {
	if o.ChainContracts != nil {
		if id, ok := o.ChainContracts[symbol]; ok {
			return id
		}
	}
	if symbol == "BTC" {
		return o.BtcContractId
	}
	return ""
}
