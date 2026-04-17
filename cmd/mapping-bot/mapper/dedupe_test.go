package mapper

import (
	"context"
	"testing"

	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
)

// --- ReconcilePendingAgainstContract -----------------------------------------------

func TestReconcilePendingAgainstContract_DropsAbsentTxs(t *testing.T) {
	bot, gql, _, state, _, _ := newTestBotWithMocks()

	// Seed local state with two sent txs.
	ctx := context.Background()
	if err := state.AddPendingTransaction(ctx, "txA", []byte("rawA"), nil); err != nil {
		t.Fatal(err)
	}
	if err := state.AddPendingTransaction(ctx, "txB", []byte("rawB"), nil); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkTransactionSent(ctx, "txA", 100); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkTransactionSent(ctx, "txB", 100); err != nil {
		t.Fatal(err)
	}

	// Contract: only txB still pending.
	gql.setPendingTxIds("txB")

	if err := bot.ReconcilePendingAgainstContract(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	metrics := bot.Metrics()
	if got := metrics.ConfirmSpendCollisions(); got != 1 {
		t.Fatalf("expected 1 confirmSpend collision, got %d", got)
	}
	if got := metrics.ReconciledDrops(); got != 1 {
		t.Fatalf("expected 1 reconciled drop, got %d", got)
	}

	// txA should be confirmed locally, txB still sent.
	sent, err := state.GetSentTransactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].TxID != "txB" {
		t.Fatalf("expected only txB still sent, got %+v", sent)
	}
}

func TestReconcilePendingAgainstContract_NoopWhenAllStillPending(t *testing.T) {
	bot, gql, _, state, _, _ := newTestBotWithMocks()
	ctx := context.Background()

	if err := state.AddPendingTransaction(ctx, "txA", []byte("rawA"), nil); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkTransactionSent(ctx, "txA", 100); err != nil {
		t.Fatal(err)
	}
	gql.setPendingTxIds("txA")

	if err := bot.ReconcilePendingAgainstContract(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := bot.Metrics().ReconciledDrops(); got != 0 {
		t.Fatalf("expected no reconciled drops, got %d", got)
	}
	sent, _ := state.GetSentTransactions(ctx)
	if len(sent) != 1 {
		t.Fatalf("expected txA still sent, got %d entries", len(sent))
	}
}

func TestReconcilePendingAgainstContract_EmptyLocalQueueIsNoop(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	if err := bot.ReconcilePendingAgainstContract(context.Background()); err != nil {
		t.Fatalf("reconcile empty: %v", err)
	}
	if got := bot.Metrics().ConfirmSpendCollisions(); got != 0 {
		t.Fatalf("unexpected collision counter increment on empty queue: %d", got)
	}
}

// --- ContractSaysProcessed ---------------------------------------------------------

func TestContractSaysProcessed_MatchesRegistry(t *testing.T) {
	bot, gql, _, _, _, _ := newTestBotWithMocks()
	gql.setPendingTxIds("pending-tx")

	ctx := context.Background()
	processed, err := bot.ContractSaysProcessed(ctx, "pending-tx")
	if err != nil {
		t.Fatal(err)
	}
	if processed {
		t.Fatal("pending tx must NOT be considered processed")
	}

	processed, err = bot.ContractSaysProcessed(ctx, "not-in-registry")
	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("tx absent from registry must be considered processed")
	}
}

// --- CollisionMetrics --------------------------------------------------------------

func TestMetrics_RecordersAreAtomicAndMonotonic(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	m := bot.Metrics()

	m.recordPendingCollision()
	m.recordPendingCollision()
	m.recordConfirmSpendCollision()
	m.recordReconciledDrop()
	m.recordReconciledDrop()
	m.recordReconciledDrop()

	if got, want := m.PendingCollisions(), uint64(2); got != want {
		t.Fatalf("PendingCollisions = %d, want %d", got, want)
	}
	if got, want := m.ConfirmSpendCollisions(), uint64(1); got != want {
		t.Fatalf("ConfirmSpendCollisions = %d, want %d", got, want)
	}
	if got, want := m.ReconciledDrops(), uint64(3); got != want {
		t.Fatalf("ReconciledDrops = %d, want %d", got, want)
	}

	// Metrics() must return the same pointer across calls.
	if bot.Metrics() != m {
		t.Fatal("Metrics() returned different pointer on second call")
	}
}

// --- RecordPendingCollision ---------------------------------------------------------

func TestRecordPendingCollision_IncrementsCounter(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	bot.RecordPendingCollision("txX")
	bot.RecordPendingCollision("txY")
	if got, want := bot.Metrics().PendingCollisions(), uint64(2); got != want {
		t.Fatalf("PendingCollisions = %d, want %d", got, want)
	}
}

// --- FetchPendingTxSpendIds via mock ------------------------------------------------

func TestFetchPendingTxSpendIds_ReturnsMockSet(t *testing.T) {
	_, gql, _, _, _, _ := newTestBotWithMocks()
	gql.setPendingTxIds("a", "b", "c")
	ctx := context.Background()
	ids, err := gql.FetchPendingTxSpendIds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 ids, got %d", len(ids))
	}
	for _, want := range []string{"a", "b", "c"} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("missing id %q", want)
		}
	}
}

// --- ensure dedupe reconciliation does not touch unrelated telemetry --------------

// mirrors Lean `applyHeartbeat_preserves_submission_accounting` / Dedupe
// analogue: reconciliation should only touch confirmSpend + reconciled drop
// counters, never the pending-collision counter.
func TestReconcile_PreservesPendingCollisionCounter(t *testing.T) {
	bot, gql, _, state, _, _ := newTestBotWithMocks()
	ctx := context.Background()

	if err := state.AddPendingTransaction(ctx, "txA", []byte("rawA"), nil); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkTransactionSent(ctx, "txA", 100); err != nil {
		t.Fatal(err)
	}
	gql.setPendingTxIds() // txA absent → collision expected

	if err := bot.ReconcilePendingAgainstContract(ctx); err != nil {
		t.Fatal(err)
	}
	if got := bot.Metrics().PendingCollisions(); got != 0 {
		t.Fatalf("PendingCollisions must not be touched by reconciliation, got %d", got)
	}
	if got := bot.Metrics().ConfirmSpendCollisions(); got != 1 {
		t.Fatalf("ConfirmSpendCollisions = %d, want 1", got)
	}
}

// Compile-time assertion that mockStateStore satisfies what we need here.
var _ interface {
	AddPendingTransaction(context.Context, string, []byte, []contractinterface.UnsignedSigHash) error
} = (*mockStateStore)(nil)
