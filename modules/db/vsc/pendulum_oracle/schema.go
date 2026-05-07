package pendulum_oracle

// SnapshotRecord is the persisted, consensus-grade integer form of an oracle
// tick produced by the Magi pendulum FeedTracker. Every numeric field is a
// raw integer (HBD/HIVE base units, precision 3) or a basis-point integer
// (BpsScale = 10000); no fixed-point fixed-decimal abstraction is needed
// outside the registry because the pendulum math operates directly on
// int64 + big.Int.
//
// WitnessRewardReductions is a sorted slice (NOT a map) so the bson byte
// sequence is deterministic across nodes.
//
// Geometry fields V/P/E/T/SBps are populated when the tick is produced and
// pin the inputs the swap-time SDK method consumes. Storing them inline keeps
// the SDK method's deterministic computation reading from a single record.
//
// Indexed by TickBlockHeight (unique).
type SnapshotRecord struct {
	TickBlockHeight uint64 `bson:"tick_block_height"`

	// HBD per HIVE in basis points.
	TrustedHivePriceBps int64 `bson:"trusted_hive_price_bps"`
	TrustedHiveOK       bool  `bson:"trusted_hive_ok"`

	HiveMovingAvgBps int64 `bson:"hive_moving_avg_bps"`
	HiveMovingAvgOK  bool  `bson:"hive_moving_avg_ok"`

	HBDInterestRateBps int  `bson:"hbd_interest_rate_bps"`
	HBDInterestRateOK  bool `bson:"hbd_interest_rate_ok"`

	TrustedWitnessGroup []string `bson:"trusted_witness_group"`

	// WitnessRewardReductions holds the per-witness liveness penalty for this
	// tick, in basis points, AFTER per-tick max-of across the 5 signal types
	// and per-tick clamp. Sorted lexicographically by Witness for byte-stable
	// bson encoding. Per-signal raw bps are kept on the same record for
	// observability (GraphQL surface, dashboards) but are NOT consumed by the
	// settlement math — only the consolidated Bps value is.
	WitnessRewardReductions []WitnessRewardReductionRecord `bson:"witness_reward_reductions"`

	// Pendulum geometry. All money-side fields are HBD/HIVE base units (int64).
	// SBps is V/E in basis points. GeometryOK gates whether the SDK method may
	// consume them — early-life snapshots before bond data exists will set
	// OK=false and the SDK call rejects with ErrSnapshotUnavailable.
	GeometryOK   bool  `bson:"geometry_ok"`
	GeometryV    int64 `bson:"geometry_v_hbd"`  // total vault value (HBD base units)
	GeometryP    int64 `bson:"geometry_p_hbd"`  // pooled HBD-side depth, summed across whitelisted pools
	GeometryE    int64 `bson:"geometry_e_hbd"`  // effective bond, T·hivePriceHBD·effectiveFraction
	GeometryT    int64 `bson:"geometry_t_hive"` // total HIVE_CONSENSUS bond across committee
	GeometrySBps int64 `bson:"geometry_s_bps"`  // ratio s = V/E in basis points
}

// WitnessRewardReductionRecord is one row of the sorted reward-reduction list.
// Bps is the consolidated per-tick value (post max-of, post-clamp) used by
// settlement math. The Evidence struct exposes per-signal raw bps for
// observability — these sub-values are not summed (the consolidated Bps is
// the max-of, not the sum) and the settlement math ignores them.
type WitnessRewardReductionRecord struct {
	Witness  string                  `bson:"witness"`
	Bps      int                     `bson:"bps"`
	Evidence WitnessLivenessEvidence `bson:"evidence"`
}

// WitnessLivenessEvidence breaks the per-tick reduction into its 5 underlying
// signals. Stored only for explorers / debugging — the settlement orchestrator
// reads Bps, never Evidence. Kept on the snapshot so a witness who disputes
// their reduction can see exactly which signals contributed.
type WitnessLivenessEvidence struct {
	BlockProductionBps         int `bson:"block_production_bps"`
	BlockAttestationBps        int `bson:"block_attestation_bps"`
	TssReshareExclusionBps     int `bson:"tss_reshare_exclusion_bps"`
	TssBlameBps                int `bson:"tss_blame_bps"`
	TssSignNonParticipationBps int `bson:"tss_sign_non_participation_bps"`
}
