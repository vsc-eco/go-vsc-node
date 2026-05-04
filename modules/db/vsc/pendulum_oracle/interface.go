package pendulum_oracle

import a "vsc-node/modules/aggregate"

type PendulumOracleSnapshots interface {
	a.Plugin

	// SaveSnapshot upserts the per-tick snapshot keyed by TickBlockHeight.
	SaveSnapshot(rec SnapshotRecord) error

	// GetSnapshotAtOrBefore returns the snapshot whose TickBlockHeight is the
	// greatest value less than or equal to blockHeight. (false, nil) if none.
	GetSnapshotAtOrBefore(blockHeight uint64) (*SnapshotRecord, bool, error)

	// GetSnapshot returns the snapshot at exactly tickBlockHeight, if any.
	GetSnapshot(tickBlockHeight uint64) (*SnapshotRecord, bool, error)

	// GetSnapshotsInRange returns every snapshot whose TickBlockHeight is in
	// the half-open interval (fromBlockHeight, toBlockHeight], sorted by
	// TickBlockHeight ascending. The boundary is exclusive on the low end so
	// the W5 settlement leader can read "(prev_epoch_start, bh-1]" without
	// double-counting the prior epoch's last tick.
	GetSnapshotsInRange(fromBlockHeight, toBlockHeight uint64) ([]SnapshotRecord, error)
}
