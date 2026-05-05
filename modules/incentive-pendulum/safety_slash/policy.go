// Package safetyslash defines verifiable safety-fault principal slashing (HIVE_CONSENSUS debit)
// distinct from liveness reward-reduction bps in the pendulum rewards path.
//
// Evidence kinds are stable string keys stored in ledger metadata and mirrored in Lean.
package safetyslash

// CorrelatedSlashCapBps is the maximum total principal slash in basis points
// applied from one correlation group (e.g. same L1 tx or same block height),
// similar in spirit to Ethereum's correlated attestation penalties.
const CorrelatedSlashCapBps = 10000

const (
	// EvidenceSettlementPayloadFraud: elected leader signed vsc.pendulum_settlement
	// that fails deterministic replay against canonical chain state.
	EvidenceSettlementPayloadFraud = "settlement_payload_fraud"
)

// Default slash severities (basis points of current HIVE_CONSENSUS bond).
const (
	// SettlementFraudSlashBps penalizes signing an objectively invalid settlement body.
	SettlementFraudSlashBps = 1000 // 10%
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
