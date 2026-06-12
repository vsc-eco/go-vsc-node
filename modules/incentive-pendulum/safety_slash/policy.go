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
// Wrongful slashes are corrected (Polkadot-style) by the vsc.safety_slash_reverse
// block-content chain op, gated by the carrying block's 2/3 BLS aggregate:
// during the challenge window it cancels the pending residual and re-credits the
// bond. Once the residual matures it lands in the keyless insurance reserve
// (ProtocolSlashReserveAccount) — a no-owner INSURANCE backstop (not V/E
// collateral; the bond is collateral) with no extraction path; only
// state-engine rules ever wrote it and nothing moves it out.
package safetyslash

// Principal (HIVE_CONSENSUS bond) slashing is gated by a per-network ACTIVATION
// HEIGHT, not a compile-time flag: see params.ConsensusParams.SafetySlashActive /
// SafetySlashActivationHeight and the single enforcing call in
// StateEngine.slashForEvidenceIfPolicyAllows. Below the height (and when 0 ==
// inert) detectors still run and log but never debit a bond, so the downstream
// reserve/reversal machinery stays dormant. The gate is a height rather than a
// bare bool because the slash mutates ledger state: every node must switch at
// the SAME block (pinned above chain head) or upgraded-vs-not nodes — and a
// reindex vs live history — would diverge and fork.

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

// DefaultSafetySlashBurnDelayBlocks holds the slash residual on
// params.ProtocolSlashPendingBurnAccount for this many Hive block heights
// before it is promoted to the keyless reserve (destination change — no
// longer burned). This IS the Polkadot-style challenge window: while the
// residual is pending, governance can cancel/reverse a wrongful slash (cancel
// pending + re-credit bond) by posting a vsc.safety_slash_reverse L1 op; once
// it matures into the reserve it is permanent insurance and no longer
// reversible. ~3 days at ~3s/block. Governance reviewing window length: this is
// mirrored in the Lean spec (magi-lean SafetySlashLiquidSplit.lean), so any
// change must update both sides in lockstep — 3 days may be tight for a DAO
// multisig to detect+decide+post; consider lengthening (cap is
// params.MaxSafetySlashBurnDelayBlocks ~115 days) when the slash path is
// enabled. Tests pass BurnDelayBlocks: 0 for immediate reserve commitment.
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
