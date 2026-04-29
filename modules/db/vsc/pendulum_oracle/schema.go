package pendulum_oracle

// SnapshotRecord is the persisted, consensus-grade integer form of an oracle
// tick produced by the Magi pendulum FeedTracker. It mirrors
// oracle.FeedTickSnapshotInt: TrustedHiveMean / HiveMovingAvg are SQ64
// fixed-point (10^8 scale, signed int64 storage); WitnessSlashBps is a sorted
// slice (NOT a map) so the bson byte sequence is deterministic across nodes.
//
// Geometry fields V/P/E/T/S are populated when the tick is produced and pin
// the (V, E, P, T, s) inputs the W3 swap-time SDK method consumes. Storing
// them inline keeps the SDK method's deterministic computation reading from a
// single record. See the geometry section of the testnet plan for the precise
// definitions.
//
// Indexed by TickBlockHeight (unique).
type SnapshotRecord struct {
	TickBlockHeight uint64 `bson:"tick_block_height"`

	TrustedHiveMean int64 `bson:"trusted_hive_mean_sq64"`
	TrustedHiveOK   bool  `bson:"trusted_hive_ok"`

	HiveMovingAvg   int64 `bson:"hive_moving_avg_sq64"`
	HiveMovingAvgOK bool  `bson:"hive_moving_avg_ok"`

	HBDInterestRateBps int  `bson:"hbd_interest_rate_bps"`
	HBDInterestRateOK  bool `bson:"hbd_interest_rate_ok"`

	TrustedWitnessGroup []string             `bson:"trusted_witness_group"`
	WitnessSlashBps     []WitnessSlashRecord `bson:"witness_slash_bps"`

	// Pendulum geometry (W7). All values stored in HBD base units (int64) for
	// money-side fields, SQ64 for ratios. GeometryOK gates whether the SDK
	// method may consume them — early-life snapshots before bond data exists
	// will set OK=false and the SDK call rejects with ErrSnapshotUnavailable.
	GeometryOK bool  `bson:"geometry_ok"`
	GeometryV  int64 `bson:"geometry_v_hbd"`        // total vault value (HBD base units)
	GeometryP  int64 `bson:"geometry_p_hbd"`        // pooled HBD-side depth, summed across whitelisted pools
	GeometryE  int64 `bson:"geometry_e_hbd"`        // effective bond, T·hivePriceHBD·effectiveFraction
	GeometryT  int64 `bson:"geometry_t_hive"`       // total HIVE_CONSENSUS bond across committee
	GeometryS  int64 `bson:"geometry_s_sq64"`       // ratio s = V/E in SQ64
}

// WitnessSlashRecord is one row of the sorted slash list.
type WitnessSlashRecord struct {
	Witness string `bson:"witness"`
	Bps     int    `bson:"bps"`
}
