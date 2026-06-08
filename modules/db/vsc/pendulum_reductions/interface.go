package pendulum_reductions

import a "vsc-node/modules/aggregate"

// PendulumReductions stores the per-account reward-reduction breakdowns the
// state engine derives at settlement-apply time. It is a local explorer /
// diagnostic cache — NOT consensus state (see PendulumReductionRecord) —
// indexed by account so a frontend can answer "why was this node reduced" the
// same way findLedgerTXs answers "what was this node paid".
type PendulumReductions interface {
	a.Plugin

	// SaveReductions idempotently upserts a batch of records (one per account)
	// for a settled epoch, keyed on PendulumReductionRecord.Id.
	SaveReductions(recs []PendulumReductionRecord) error

	// FindReductions returns reduction rows matching the filter, sorted by
	// epoch descending then account ascending, with offset/limit applied. A
	// nil filter field is unconstrained; epoch (exact) takes precedence over
	// the fromEpoch/toEpoch range.
	FindReductions(
		account *string,
		epoch *uint64,
		fromEpoch *uint64,
		toEpoch *uint64,
		offset int,
		limit int,
	) ([]PendulumReductionRecord, error)

	// HasEpoch reports whether any reduction rows exist for an epoch. Used to
	// gate apply-time persistence (and a future backfill pass) so re-applying
	// an already-recorded epoch is a no-op.
	HasEpoch(epoch uint64) (bool, error)
}
