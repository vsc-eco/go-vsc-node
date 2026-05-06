// Package safetyslash defines verifiable safety-fault principal slashing (HIVE_CONSENSUS debit)
// distinct from liveness reward-reduction bps in the pendulum rewards path.
//
// Design intent: this is a node-wide policy surface — reward reductions stay tied to
// pendulum distribution math, but principal safety slashes should eventually be triggered
// from any provable fault path (TSS equivocation, conflicting block signatures, invalid
// oracle attestations, fraudulent settlement bodies, etc.). Only a subset is wired in Go
// today; each new detector should call the same ledger entrypoint with a stable evidence
// kind string and correlated BPS composition.
//
// Evidence kinds are stable string keys stored in ledger metadata and mirrored in Lean.
// Reserved (unwired) examples for future detectors — do not reuse strings:
//
//	"tss_equivocation" — conflicting TSS aggregate signatures for the same logical round
//	"vsc_double_block_sign" — same witness key signs two canonical block hashes at one height
//	"oracle_payload_fraud" — signed oracle payload disagrees with deterministic replay
//
// Reversal / governance undo of pending burn + consensus credit is not implemented yet;
// delayed burn exists so protocol bugs can be corrected before maturity.
package safetyslash

// CorrelatedSlashCapBps is the maximum total principal slash in basis points
// applied from one correlation group (e.g. same L1 tx or same block height),
// similar in spirit to Ethereum's correlated attestation penalties.
const CorrelatedSlashCapBps = 10000

const (
	// EvidenceSettlementPayloadFraud: elected leader signed vsc.pendulum_settlement
	// that fails deterministic replay against canonical chain state (Hive L1 custom_json
	// ingress — see state_engine.handlePendulumSettlement). This is distinct from W3
	// system.pendulum_apply_swap_fees, which only validates swap fee math against oracle snapshots.
	EvidenceSettlementPayloadFraud = "settlement_payload_fraud"
	// EvidenceTSSEquivocation: node emits conflicting signed TSS commitments/shares
	// for the same logical session/round, evidenced on-chain.
	EvidenceTSSEquivocation = "tss_equivocation"
	// EvidenceVSCDoubleBlockSign: proposer signs competing VSC blocks at one slot height.
	EvidenceVSCDoubleBlockSign = "vsc_double_block_sign"
	// EvidenceVSCInvalidBlockProposal: proposer submits block that fails deterministic replay checks.
	EvidenceVSCInvalidBlockProposal = "vsc_invalid_block_proposal"
	// EvidenceOraclePayloadFraud: witness feed attestation diverges from deterministic canonical feed view.
	EvidenceOraclePayloadFraud = "oracle_payload_fraud"
)

// Default slash severities (basis points of current HIVE_CONSENSUS bond).
const (
	// SettlementFraudSlashBps penalizes signing an objectively invalid settlement body.
	SettlementFraudSlashBps = 1000 // 10%
	TSSEquivocationSlashBps = 1000 // 10%
	DoubleBlockSignSlashBps = 1000 // 10%
	InvalidBlockSlashBps    = 1000 // 10%
	OraclePayloadSlashBps   = 1000 // 10%
)

// Threshold policy (hybrid default):
//   - immediate slash on deterministic proof: settlement, double-block, invalid-block
//   - thresholded slash: tss equivocation, oracle payload fraud
const (
	TSSEquivocationThresholdCount = 2
	OraclePayloadThresholdCount   = 2
	EvidenceThresholdWindowBlocks = 1000
	OracleDivergenceThresholdBps  = 300
)

// DefaultSafetySlashBurnDelayBlocks holds the burn (post-restitution) portion on
// params.ProtocolSlashPendingBurnAccount for this many Hive block heights before
// it is promoted to the final burn sink. ~3 days at ~3s/block. Governance can
// later reverse a slash (consensus credit + cancel pending) before maturity.
// Tests should pass BurnDelayBlocks: 0 for immediate burn.
// Values above params.MaxSafetySlashBurnDelayBlocks are clamped when slashing.
const DefaultSafetySlashBurnDelayBlocks uint64 = 3 * 28800

// EffectiveCorrelatedBps sums positive raw contributions and caps at capBps.
// Use when multiple evidence lines apply to the same incident.
func EffectiveCorrelatedBps(rawParts []int, capBps int) int {
	if capBps <= 0 {
		return 0
	}
	sum := 0
	for _, x := range rawParts {
		if x > 0 {
			sum += x
		}
	}
	if sum > capBps {
		return capBps
	}
	return sum
}

func SlashBpsForEvidenceKind(kind string) int {
	switch kind {
	case EvidenceSettlementPayloadFraud:
		return SettlementFraudSlashBps
	case EvidenceTSSEquivocation:
		return TSSEquivocationSlashBps
	case EvidenceVSCDoubleBlockSign:
		return DoubleBlockSignSlashBps
	case EvidenceVSCInvalidBlockProposal:
		return InvalidBlockSlashBps
	case EvidenceOraclePayloadFraud:
		return OraclePayloadSlashBps
	default:
		return 0
	}
}

func UsesThreshold(kind string) bool {
	switch kind {
	case EvidenceTSSEquivocation, EvidenceOraclePayloadFraud:
		return true
	default:
		return false
	}
}

func ThresholdCountForEvidenceKind(kind string) int {
	switch kind {
	case EvidenceTSSEquivocation:
		return TSSEquivocationThresholdCount
	case EvidenceOraclePayloadFraud:
		return OraclePayloadThresholdCount
	default:
		return 1
	}
}
