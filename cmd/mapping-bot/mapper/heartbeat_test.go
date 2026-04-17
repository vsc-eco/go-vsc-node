package mapper

import (
	"context"
	"testing"
	"time"
)

func TestObserveHeartbeat_BasicInsert(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()

	err := bot.ObserveHeartbeat(BotEndpoint{
		Operator:   "did:pkh:eip155:peer-1",
		Endpoint:   "https://peer-1.example:8080",
		ObservedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	live := bot.LiveOperators(time.Now(), 0)
	if len(live) != 1 {
		t.Fatalf("expected 1 live operator, got %d", len(live))
	}
	if live[0].Operator != "did:pkh:eip155:peer-1" {
		t.Fatalf("unexpected operator: %q", live[0].Operator)
	}
	if live[0].Endpoint != "https://peer-1.example:8080" {
		t.Fatalf("unexpected endpoint: %q", live[0].Endpoint)
	}
}

func TestObserveHeartbeat_RejectsEmptyOperator(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	if err := bot.ObserveHeartbeat(BotEndpoint{Operator: ""}); err == nil {
		t.Fatal("expected error for empty operator")
	}
	if got := len(bot.LiveOperators(time.Now(), 0)); got != 0 {
		t.Fatalf("expected empty directory after rejected insert, got %d", got)
	}
}

func TestObserveHeartbeat_PreservesEndpointAcrossRefresh(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()

	t0 := time.Now().Add(-30 * time.Second)
	if err := bot.ObserveHeartbeat(BotEndpoint{
		Operator:   "op-A",
		Endpoint:   "https://first.example",
		ObservedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now()
	if err := bot.ObserveHeartbeat(BotEndpoint{
		Operator:   "op-A",
		Endpoint:   "", // intentionally missing on refresh
		ObservedAt: t1,
	}); err != nil {
		t.Fatal(err)
	}

	live := bot.LiveOperators(t1, 0)
	if len(live) != 1 {
		t.Fatalf("expected 1 live operator, got %d", len(live))
	}
	if live[0].Endpoint != "https://first.example" {
		t.Fatalf("endpoint was lost on refresh: %q", live[0].Endpoint)
	}
	if !live[0].ObservedAt.Equal(t1) {
		t.Fatalf("ObservedAt not bumped by refresh: %v vs %v", live[0].ObservedAt, t1)
	}
}

func TestLiveOperators_StaleFilteredOut(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	now := time.Now()

	if err := bot.ObserveHeartbeat(BotEndpoint{
		Operator:   "fresh",
		ObservedAt: now.Add(-10 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := bot.ObserveHeartbeat(BotEndpoint{
		Operator:   "stale",
		ObservedAt: now.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	live := bot.LiveOperators(now, 0)
	if len(live) != 1 {
		t.Fatalf("expected only fresh heartbeat to be live, got %d", len(live))
	}
	if live[0].Operator != "fresh" {
		t.Fatalf("unexpected live operator: %q", live[0].Operator)
	}
}

func TestPruneStaleHeartbeats_DropsOldEntries(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	now := time.Now()

	_ = bot.ObserveHeartbeat(BotEndpoint{Operator: "a", ObservedAt: now.Add(-3 * time.Minute)})
	_ = bot.ObserveHeartbeat(BotEndpoint{Operator: "b", ObservedAt: now.Add(-5 * time.Second)})
	_ = bot.ObserveHeartbeat(BotEndpoint{Operator: "c", ObservedAt: now.Add(-10 * time.Minute)})

	pruned := bot.PruneStaleHeartbeats(now, 0)
	if pruned != 2 {
		t.Fatalf("expected 2 pruned, got %d", pruned)
	}
	live := bot.LiveOperators(now, 0)
	if len(live) != 1 || live[0].Operator != "b" {
		t.Fatalf("expected only operator b to remain, got %+v", live)
	}
}

func TestLiveOperators_DeterministicOrder(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	now := time.Now()

	for _, op := range []string{"zeta", "alpha", "mu", "beta"} {
		_ = bot.ObserveHeartbeat(BotEndpoint{Operator: op, ObservedAt: now})
	}

	live := bot.LiveOperators(now, 0)
	want := []string{"alpha", "beta", "mu", "zeta"}
	if len(live) != len(want) {
		t.Fatalf("expected %d entries, got %d", len(want), len(live))
	}
	for i, op := range want {
		if live[i].Operator != op {
			t.Fatalf("live[%d] = %q, want %q", i, live[i].Operator, op)
		}
	}
}

func TestExportImportSnapshot_RoundTrip(t *testing.T) {
	botA, _, _, _, _, _ := newTestBotWithMocks()
	botB, _, _, _, _, _ := newTestBotWithMocks()

	now := time.Now()
	_ = botA.ObserveHeartbeat(BotEndpoint{Operator: "a", Endpoint: "ep-a", LastBlockHeight: 42, ObservedAt: now})
	_ = botA.ObserveHeartbeat(BotEndpoint{Operator: "b", Endpoint: "ep-b", LastBlockHeight: 43, ObservedAt: now})

	snap, err := botA.ExportHeartbeatSnapshot()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := botB.ImportHeartbeatSnapshot(snap); err != nil {
		t.Fatalf("import: %v", err)
	}

	live := botB.LiveOperators(now, 0)
	if len(live) != 2 {
		t.Fatalf("expected 2 entries after import, got %d", len(live))
	}
	if live[0].Operator != "a" || live[1].Operator != "b" {
		t.Fatalf("unexpected import order: %+v", live)
	}
	if live[0].LastBlockHeight != 42 || live[1].LastBlockHeight != 43 {
		t.Fatalf("block heights not preserved: %+v", live)
	}
}

func TestImportHeartbeatSnapshot_SkipsEmptyOperator(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	raw := []byte(`[{"operator":""},{"operator":"ok","observed_at":"2026-01-01T00:00:00Z"}]`)
	if err := bot.ImportHeartbeatSnapshot(raw); err != nil {
		t.Fatalf("import: %v", err)
	}
	// Use a very generous staleness window so the 2026-01-01 timestamp
	// still counts as fresh relative to any reasonable test-run clock.
	live := bot.LiveOperators(time.Now(), 10*365*24*time.Hour)
	if len(live) != 1 || live[0].Operator != "ok" {
		t.Fatalf("expected only 'ok' operator, got %+v", live)
	}
}

func TestImportHeartbeatSnapshot_EmptyPayload(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	if err := bot.ImportHeartbeatSnapshot(nil); err != nil {
		t.Fatalf("import nil: %v", err)
	}
	if err := bot.ImportHeartbeatSnapshot([]byte{}); err != nil {
		t.Fatalf("import empty: %v", err)
	}
	if got := len(bot.LiveOperators(time.Now(), 0)); got != 0 {
		t.Fatalf("expected empty directory, got %d", got)
	}
}

func TestBroadcastHeartbeat_PopulatesLastBlock(t *testing.T) {
	bot, _, _, _, _, _ := newTestBotWithMocks()
	bot.setLastBlock(1234)
	hb := bot.BroadcastHeartbeat(context.Background(), "https://me.example")
	if hb.LastBlockHeight != 1234 {
		t.Fatalf("expected LastBlockHeight=1234, got %d", hb.LastBlockHeight)
	}
	if hb.Endpoint != "https://me.example" {
		t.Fatalf("unexpected endpoint: %q", hb.Endpoint)
	}
	if hb.ObservedAt.IsZero() {
		t.Fatal("ObservedAt must be populated")
	}
}

// Concretization check for Lean
// `applyHeartbeat_preserves_submission_accounting` /
// `heartbeat_trace_preserves_submission_accounting`:
// observing any sequence of heartbeats MUST NOT touch submission-accounting
// counters (collision metrics) or local StateDB sent/pending queues.
func TestHeartbeat_TracePreservesSubmissionAccounting(t *testing.T) {
	bot, _, _, state, _, _ := newTestBotWithMocks()

	pendingBefore := bot.Metrics().PendingCollisions()
	confirmBefore := bot.Metrics().ConfirmSpendCollisions()
	droppedBefore := bot.Metrics().ReconciledDrops()
	sentBefore, err := state.GetSentTransactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	for i := 0; i < 50; i++ {
		_ = bot.ObserveHeartbeat(BotEndpoint{
			Operator:   "op-" + string(rune('a'+i%26)),
			ObservedAt: now.Add(time.Duration(-i) * time.Second),
		})
	}
	_ = bot.PruneStaleHeartbeats(now, 5*time.Second)

	if got := bot.Metrics().PendingCollisions(); got != pendingBefore {
		t.Fatalf("PendingCollisions changed by heartbeat trace: %d -> %d", pendingBefore, got)
	}
	if got := bot.Metrics().ConfirmSpendCollisions(); got != confirmBefore {
		t.Fatalf("ConfirmSpendCollisions changed by heartbeat trace: %d -> %d", confirmBefore, got)
	}
	if got := bot.Metrics().ReconciledDrops(); got != droppedBefore {
		t.Fatalf("ReconciledDrops changed by heartbeat trace: %d -> %d", droppedBefore, got)
	}
	sentAfter, err := state.GetSentTransactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sentAfter) != len(sentBefore) {
		t.Fatalf("StateDB sent queue mutated by heartbeat trace: len %d -> %d",
			len(sentBefore), len(sentAfter))
	}
}
