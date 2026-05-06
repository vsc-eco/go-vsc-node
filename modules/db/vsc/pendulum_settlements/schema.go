package pendulum_settlements

// SettlementMarker records that a settlement record for `Epoch` has been
// processed by the state engine. It is the on-chain authority used by the
// election proposer (`canHold`) and the state engine's `vsc.election_result`
// handler to enforce that no new election proceeds without prior settlement.
//
// The full settlement payload is recoverable from the VSC block that carried
// the BlockTypePendulumSettlement tx; this collection only stores the
// summary needed by the cross-component guards.
type SettlementMarker struct {
	Epoch               uint64 `bson:"epoch"`
	BlockHeight         uint64 `bson:"block_height"`
	SnapshotRangeFrom   uint64 `bson:"snapshot_range_from"`
	SnapshotRangeTo     uint64 `bson:"snapshot_range_to"`
	TotalDistributedHBD int64  `bson:"total_distributed_hbd"`
	ResidualHBD         int64  `bson:"residual_hbd"`
}
