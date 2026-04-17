package mapper

// Contract-backed cross-operator dedupe for the mapping bot.
//
// The `TxSpendsRegistry` state key on the BTC mapping contract is the
// authoritative list of "pending unmap txs still awaiting confirmSpend". It
// is mutated only by consensus-driven state transitions (a new spend being
// registered, or `HandleConfirmSpend` succeeding and removing an entry).
// Every bot operator that queries the registry therefore sees the same set
// at a given contract height.
//
// The bot's local Mongo (`IsTransactionProcessed`, `GetSentTransactions`,
// etc.) is now a *cache* for per-operator work-queue state, not a dedupe
// gate. When the contract registry disagrees with local state, the contract
// wins:
//
//   - txId present in local sent-queue AND absent from contract registry
//     → another operator already succeeded; log a collision and mark the
//       local entry confirmed, dropping it from the work queue.
//   - txId absent from local state but present in contract registry
//     → normal new-spend ingestion, nothing to reconcile.
//   - txId present in both
//     → pending as expected; continue accumulating signatures / waiting
//       for BTC confirmation.
//
// This mirrors the Lean `Security.MappingBot` theorems:
//   - `contractSubmit_idempotent`               — duplicate confirmSpend is a no-op
//   - `contractSubmit_operator_independent`     — operators converge on the same state
//   - `contractSubmit_monotone`                 — registry entries never return once removed
//
// See `AUDIT.md` §"Mapping-bot decentralization boundary".

import (
	"context"
	"sync/atomic"
)

// CollisionMetrics tracks cross-operator dedupe observations. Counters are
// monotonic and lock-free — all reads/writes go through atomics so the
// metrics surface can be polled from Prometheus or the bot's HTTP admin
// endpoint without serializing the hot path.
type CollisionMetrics struct {
	// PendingCollisions counts tx-spends observed in incoming registry
	// updates that the LOCAL database already considers processed. These
	// are benign — another operator registered the spend first.
	pendingCollisions atomic.Uint64
	// ConfirmSpendCollisions counts sent-txs that disappeared from the
	// contract's pending registry before this operator's confirmSpend ran.
	// These are also benign — another operator's confirmSpend landed first.
	confirmSpendCollisions atomic.Uint64
	// ReconciledDrops counts local sent-queue entries that were force-
	// confirmed via reconciliation rather than a BTC confirmation path.
	reconciledDrops atomic.Uint64
}

// PendingCollisions returns the current pending-collision counter.
func (c *CollisionMetrics) PendingCollisions() uint64 { return c.pendingCollisions.Load() }

// ConfirmSpendCollisions returns the current confirmSpend-collision counter.
func (c *CollisionMetrics) ConfirmSpendCollisions() uint64 {
	return c.confirmSpendCollisions.Load()
}

// ReconciledDrops returns the total number of local entries dropped by
// contract-state reconciliation.
func (c *CollisionMetrics) ReconciledDrops() uint64 { return c.reconciledDrops.Load() }

func (c *CollisionMetrics) recordPendingCollision()      { c.pendingCollisions.Add(1) }
func (c *CollisionMetrics) recordConfirmSpendCollision() { c.confirmSpendCollisions.Add(1) }
func (c *CollisionMetrics) recordReconciledDrop()        { c.reconciledDrops.Add(1) }

// Metrics exposes the bot's collision metrics. Accessor so tests and
// callers do not reach into internals. Safe to call before `Init`.
func (b *Bot) Metrics() *CollisionMetrics {
	b.metricsOnce.Do(func() {
		b.collisionMetrics = &CollisionMetrics{}
	})
	return b.collisionMetrics
}

// ReconcilePendingAgainstContract walks the bot's local sent-transaction
// queue and drops any entry whose txId no longer appears in the contract's
// `TxSpendsRegistry`. Such absences are authoritative proof that some
// operator (possibly this one, possibly a peer) already landed
// `confirmSpend` on-chain, so re-broadcasting would be wasted work at best
// and a double-submission collision at worst.
//
// This is purely advisory for local-DB hygiene: correctness does NOT depend
// on it (the contract-side `HandleConfirmSpend` is already idempotent). Its
// purpose is (a) to stop retrying already-finished txs and (b) to surface
// cross-operator collision telemetry via `CollisionMetrics`.
func (b *Bot) ReconcilePendingAgainstContract(ctx context.Context) error {
	sent, err := b.stateDB().GetSentTransactions(ctx)
	if err != nil {
		return err
	}
	if len(sent) == 0 {
		return nil
	}
	pending, err := b.gql().FetchPendingTxSpendIds(ctx)
	if err != nil {
		return err
	}
	metrics := b.Metrics()
	for _, tx := range sent {
		if _, stillPending := pending[tx.TxID]; stillPending {
			continue
		}
		metrics.recordConfirmSpendCollision()
		metrics.recordReconciledDrop()
		b.L.Info("contract-state reconciliation: dropping locally-pending tx already absent from contract registry",
			"txId", tx.TxID,
			"collision_reason", "contract_registry_absent",
		)
		if err := b.stateDB().MarkTransactionConfirmed(ctx, tx.TxID); err != nil {
			b.L.Warn("failed to mark tx confirmed during reconciliation",
				"txId", tx.TxID, "error", err)
			// Continue — a future reconciliation pass will retry.
		}
	}
	return nil
}

// ContractSaysProcessed reports whether the contract's current registry view
// considers `txId` finished (i.e., not in the pending set). This is the
// authoritative dedupe gate for deciding whether to perform contract-state-
// mutating work against `txId`. Unlike `StateStore.IsTransactionProcessed`
// (which reads local Mongo), this query is consistent across all operators
// at the same contract height.
//
// Usage caveat: a "processed" result does NOT mean this operator's local
// state is stale — the registry can simply lag behind ingestion, or the
// txId may predate the bot's view. Callers should treat this as a "skip
// further contract mutations" signal, not a blanket "erase local state"
// instruction.
func (b *Bot) ContractSaysProcessed(ctx context.Context, txId string) (bool, error) {
	pending, err := b.gql().FetchPendingTxSpendIds(ctx)
	if err != nil {
		return false, err
	}
	_, stillPending := pending[txId]
	return !stillPending, nil
}

// RecordPendingCollision is called by `ProcessTxSpends` when the contract
// offers a txId the local cache already considers processed. Exported for
// tests and for future ingestion paths that want to emit the same
// telemetry.
func (b *Bot) RecordPendingCollision(txId string) {
	b.Metrics().recordPendingCollision()
	b.L.Debug("pending-spend collision: contract re-offered tx already in local processed cache",
		"txId", txId,
	)
}
