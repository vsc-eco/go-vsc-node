// Package safetyslash defines verifiable safety-fault principal slashing
// (HIVE_CONSENSUS debit) distinct from liveness reward-reduction bps in the
// pendulum rewards path.
//
// Scope (current): the only on-chain provable safety violations available to a
// replaying node are about block production/signing — every other candidate
// (TSS commitment blame, oracle quote divergence, settlement-body replay) is
// either now block-internal and BLS-signed (so block-level rejection already
// covers it) or fundamentally a liveness signal that cannot be distinguished
// from honest p2p outages on-chain. Those still feed the liveness reward
// reduction path; they intentionally do NOT trigger a principal slash here.
//
// Reserved (unwired) kinds — do not reuse the strings — that may return when
// the underlying detector becomes deterministically provable from chain data:
//
//	"tss_equivocation"          — divergent TSS shares for the same logical round
//	"oracle_payload_fraud"      — signed oracle payload disagrees with replay
//	"settlement_payload_fraud"  — fraudulent settlement body (now caught at
//	                              block-validation time before this layer)
//
// Reversal / governance undo of pending burn + consensus credit is not yet
// wired; the delayed burn merely creates a window so protocol bugs can be
// corrected before maturity.
package safetyslash

// CorrelatedSlashCapBps is the maximum total principal slash in basis points
// applied from one correlation group (e.g. same L1 tx or same block height),
// similar in spirit to Ethereum's correlated attestation penalties.
const CorrelatedSlashCapBps = 10000

const (
	// EvidenceVSCDoubleBlockSign: proposer signs competing VSC blocks at one slot
	// height. Both signatures land on Hive L1 (or are reconstructible from there),
	// so any replaying node can deterministically prove the equivocation.
	EvidenceVSCDoubleBlockSign = "vsc_double_block_sign"
	// EvidenceVSCInvalidBlockProposal: proposer submits a block whose state
	// transitions fail deterministic re-execution (TxProposeBlock.Validate=false).
	EvidenceVSCInvalidBlockProposal = "vsc_invalid_block_proposal"
)

// Default slash severities (basis points of current HIVE_CONSENSUS bond).
const (
	DoubleBlockSignSlashBps = 1000 // 10%
	InvalidBlockSlashBps    = 1000 // 10%
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
	case EvidenceVSCDoubleBlockSign:
		return DoubleBlockSignSlashBps
	case EvidenceVSCInvalidBlockProposal:
		return InvalidBlockSlashBps
	default:
		return 0
	}
}
