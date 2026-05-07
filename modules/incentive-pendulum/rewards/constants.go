// Package rewards implements VSC committee liveness scoring.
//
// All committee members earn a share of each epoch's pendulum:nodes:HBD
// bucket pro-rata to their HIVE_CONSENSUS bond. This package computes per-tick
// reward-reduction basis points from on-chain L2 evidence (VSC blocks +
// TSS commitments) and aggregates them per-epoch into the effective bond
// weight that drives the distribution math.
//
// The system never debits principal — bonds remain unchanged on the ledger.
// True slashing for provable safety faults (equivocation, double-signing) is
// out of scope and lives in a separate workstream.
package rewards

// Per-event weights (basis points). Tune here only.
const (
	// BlockProductionMissBps applies once per VSC slot whose elected proposer
	// did not produce a block.
	BlockProductionMissBps = 200

	// BlockAttestationMissBps applies once per VSC block where a committee
	// member is absent from the BLS Signers list.
	BlockAttestationMissBps = 25

	// TssReshareExclusionBps applies once per (witness, key) pair when the
	// witness is in the elected committee but absent from the canonical
	// reshare commitment's bitset for that key in the epoch. Strict per-key:
	// being excluded from N keys accumulates N × this value before the
	// per-tick max-of and per-tick cap.
	TssReshareExclusionBps = 1000

	// TssBlameBps applies once per blame commitment that names this witness
	// in its bitset (i.e., the witness was a culprit in a session that timed
	// out or errored, forcing a retry).
	TssBlameBps = 150

	// TssSignNonParticipationBps applies once per successful sign_result
	// commitment whose BLS bitvec does not include this witness. Lighter than
	// a blame because the session still succeeded without them.
	TssSignNonParticipationBps = 30

	// OracleQuoteDivergenceBps applies once per tick when a trusted-group
	// witness's published HBD/HIVE quote diverges from the trusted-group mean
	// by at least OracleQuoteDivergenceThresholdBps. This signal is liveness,
	// not safety: a stale or out-of-sync feed is indistinguishable from a
	// fraudulent one when seen only from on-chain feed_publish data, so the
	// reward path is the appropriate response. Comparable in weight to
	// TssBlameBps because divergence persists across many sub-ticks until the
	// witness republishes.
	OracleQuoteDivergenceBps = 150
)

// OracleQuoteDivergenceThresholdBps is the minimum |quote - trusted_mean|
// (in basis points of the mean) that flags a trusted witness as divergent.
// Mirrors the legacy safetyslash.OracleDivergenceThresholdBps that drove the
// retired principal-slash detector — preserved here so the reward-reduction
// path applies the same evidence threshold.
const OracleQuoteDivergenceThresholdBps = 300

// PerTickCapBps clamps each per-signal raw bps and the post-max-of tick
// total. Defensive — prevents a single bad tick from dominating an epoch.
const PerTickCapBps = 1000

// PerEpochCapBps is the hard ceiling on accumulated reward reduction across
// the epoch. 10000 bps = 100% of share, i.e., a sufficiently delinquent
// witness can lose the entire epoch's reward share.
const PerEpochCapBps = 10000

// PerEpochForgivenessBps is a flat allowance subtracted from the accumulated
// per-epoch bps before the cap is applied. Replaces per-tick "restart grace"
// logic — a witness with isolated minor failures (e.g., one missed sign,
// one missed slot) sees their epoch bps fall below the buffer and incurs
// no effective reduction.
const PerEpochForgivenessBps = 250
