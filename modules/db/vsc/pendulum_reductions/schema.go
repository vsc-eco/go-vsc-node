package pendulum_reductions

// PendulumReductionRecord is one account's reward-reduction breakdown for a
// settled epoch — a locally-derived, queryable explanation of the single
// consolidated bps that landed in the on-chain settlement record. It is NOT
// consensus state: every honest node re-derives identical rows from the same
// on-chain L2 evidence at the settlement's snapshot heights (see
// rewards.ComputeReductionEvidenceForEpoch), so this collection is a cache the
// GraphQL explorer reads, never something that is signed or byte-compared.
//
// One document per (epoch, account). Id is "epoch:account" so the apply-time
// upsert is idempotent under restart-replay.
//
// The per-signal *Bps fields are each liveness signal's bps summed across the
// epoch's ticks (pre-forgiveness contributing evidence). They do NOT sum to
// ConsolidatedBps: the consolidated path takes the per-tick max across signals
// before summing across ticks and then subtracts the per-epoch forgiveness.
// ConsolidatedBps is the authoritative reduction (mirrors the on-chain
// RewardReductionEntry.Bps); RawConsolidatedBps is the pre-forgiveness total.
type PendulumReductionRecord struct {
	// Id is "epoch:account" — the idempotency key for apply-time upserts.
	Id string `json:"id" bson:"id"`

	Epoch   uint64 `json:"epoch" bson:"epoch"`
	Account string `json:"account" bson:"account"`

	// BlockHeight is the block the settlement was applied at.
	BlockHeight uint64 `json:"block_height" bson:"block_height"`
	// Snapshot bounds of the L2 evidence window the reductions cover
	// (SnapshotRangeFrom exclusive, SnapshotRangeTo inclusive).
	SnapshotRangeFrom uint64 `json:"snapshot_range_from" bson:"snapshot_range_from"`
	SnapshotRangeTo   uint64 `json:"snapshot_range_to" bson:"snapshot_range_to"`

	// ConsolidatedBps is the authoritative post-forgiveness, post-cap reduction
	// applied to this account's bond — equals the on-chain
	// RewardReductionEntry.Bps. 0 when the account accrued penalty signals but
	// the raw total fell at or below the per-epoch forgiveness.
	ConsolidatedBps int `json:"consolidated_bps" bson:"consolidated_bps"`
	// RawConsolidatedBps is the pre-forgiveness sum of per-tick max-of bps.
	RawConsolidatedBps int `json:"raw_consolidated_bps" bson:"raw_consolidated_bps"`

	// Per-signal contributing evidence (bps summed across ticks). See type doc.
	BlockProductionBps         int `json:"block_production_bps" bson:"block_production_bps"`
	BlockAttestationBps        int `json:"block_attestation_bps" bson:"block_attestation_bps"`
	TssReshareExclusionBps     int `json:"tss_reshare_exclusion_bps" bson:"tss_reshare_exclusion_bps"`
	TssBlameBps                int `json:"tss_blame_bps" bson:"tss_blame_bps"`
	TssSignNonParticipationBps int `json:"tss_sign_non_participation_bps" bson:"tss_sign_non_participation_bps"`
	OracleQuoteDivergenceBps   int `json:"oracle_quote_divergence_bps" bson:"oracle_quote_divergence_bps"`

	// HbdDistributed is what this account was actually paid from the
	// pendulum:nodes bucket in the same settlement (0 if not in the
	// distribution list). The canonical payout record lives in the ledger
	// (findLedgerTXs, type "pendulum_distribute"); this is a convenience join.
	HbdDistributed int64 `json:"hbd_distributed" bson:"hbd_distributed"`
}
