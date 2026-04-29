package pendulum_oracle

// SnapshotRecord is the persisted, consensus-grade integer form of an oracle
// tick produced by the Magi pendulum FeedTracker. It mirrors
// oracle.FeedTickSnapshotInt: TrustedHiveMean / HiveMovingAvg are SQ64
// fixed-point (10^8 scale, signed int64 storage); WitnessSlashBps is a sorted
// slice (NOT a map) so the bson byte sequence is deterministic across nodes.
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
}

// WitnessSlashRecord is one row of the sorted slash list.
type WitnessSlashRecord struct {
	Witness string `bson:"witness"`
	Bps     int    `bson:"bps"`
}
