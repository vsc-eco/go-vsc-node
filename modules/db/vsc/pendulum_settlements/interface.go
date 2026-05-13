package pendulum_settlements

import a "vsc-node/modules/aggregate"

// PendulumSettlements stores the per-epoch settlement markers written by the
// state engine after applying a BlockTypePendulumSettlement record. It is a
// summary index only — the full payload lives in the VSC block that carried
// the settlement tx.
type PendulumSettlements interface {
	a.Plugin

	// SaveMarker upserts a marker for the given epoch.
	SaveMarker(rec SettlementMarker) error

	// GetLatestSettledEpoch returns the highest epoch that has been settled.
	// Returns 0, false if no settlement has been applied yet.
	GetLatestSettledEpoch() (uint64, bool)

	// GetMarker returns the marker for `epoch`, if present.
	GetMarker(epoch uint64) (*SettlementMarker, bool, error)
}
