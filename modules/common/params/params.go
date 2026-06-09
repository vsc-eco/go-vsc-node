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
var ProtocolSlashBurnAccount = "system:protocol_slash_burn"

// ProtocolSlashPendingBurnAccount holds the liquid HIVE slice of a safety slash
// until BurnDelayBlocks passes, then FinalizeMaturedSafetySlashBurns moves it
// to ProtocolSlashBurnAccount. Not spendable (excluded from balance aggregation).
var ProtocolSlashPendingBurnAccount = "system:protocol_slash_burn_pending"

// ProtocolSlashFinalizeCursorAccount stores the single-row scan cursor used by
// FinalizeMaturedSafetySlashBurns. The cursor's value (in the From field) is
// the lowest emission BlockHeight at which any pending burn row could still be
// unfinalized. On every UpdateBalances tick, the finalizer scans only
// [cursor, blockHeight] instead of [0, blockHeight], so the cost is bounded by
// the active pending-row window rather than chain length. Account is balance-
// neutral (every row has Amount=0); it exists purely as a meta marker.
var ProtocolSlashFinalizeCursorAccount = "system:protocol_slash_finalize_cursor"

// ProtocolSlashRestitutionClaimsAccount stores the FIFO queue of victim
// restitution claims as ledger rows. New claims are written via the
// vsc.restitution_claim block-content tx (see modules/state-processing).
// SafetySlashConsensusBond calls OnLedgerRestitutionAllocator, which reads
// the unconsumed claim rows here, allocates HIVE FIFO, and writes consume
// markers. The account is balance-neutral (allocate writes credit the
// victim on their own account, not this meta account); it only acts as
// the queue's storage.
var ProtocolSlashRestitutionClaimsAccount = "system:protocol_slash_restitution_claims"

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
// The MAINNET rollout gate height lives in ConsensusParams.Version0_2_0Height
// (set per-network in system-config), alongside the other rollout heights.
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

	// Version0_2_0Height is the single activation height for the v0.2.0 release
	// batch — one named "fork" (Ethereum ChainConfig style) that every
	// consensus-affecting change shipping in v0.2.0 keys off via Version0_2_0Active,
	// instead of minting a separate gate per change that all carry the same value.
	//
	// The contract-update timelock is the first rule on it: on mainnet a
	// vsc.update_contract at BlockHeight >= this value is queued behind the network
	// timelock (ContractUpdateTimelockBlocks); earlier updates stay immediate so a
	// full reindex reproduces historical state byte-for-byte. 0 == not scheduled
	// (every v0.2.0 rule inert; updates immediate).
	//
	// MUST be a height STRICTLY ABOVE the current chain head when this binary is
	// deployed — a value at/below an already-processed block is a consensus footgun
	// (live nodes ran pre-v0.2.0 behavior over those blocks; a reindex would apply
	// the new behavior and diverge). CONSENSUS-CRITICAL DEPLOY CONSTRAINT: every
	// witness must run a binary carrying this height BEFORE Hive reaches it, or
	// upgraded and not-yet-upgraded nodes disagree across the gap. If the rollout
	// slips past this height, bump it before deploying. Ephemeral networks
	// (devnet/mocknet) may pin it at 1 to exercise v0.2.0 behavior from genesis.
	Version0_2_0Height uint64 `json:"version0_2_0Height,omitempty"`

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

// Version0_2_0Active reports whether the v0.2.0 release batch is in force at
// blockHeight. Every consensus-affecting change shipping in v0.2.0 gates on this
// single predicate so the network has one coordinated activation rather than a
// gate per change. Zero height means the batch is not yet scheduled (inert).
func (cp ConsensusParams) Version0_2_0Active(blockHeight uint64) bool {
	return cp.Version0_2_0Height != 0 && blockHeight >= cp.Version0_2_0Height
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
