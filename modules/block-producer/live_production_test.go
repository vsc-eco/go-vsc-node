package blockproducer

import "testing"

// TestLiveProductionAllowed_SkipsReplayedSlotsAfterRestart is the crash/replay
// double-sign guard: after a restart the consumer re-processes recent blocks, so
// BlockTick can re-reach an already-attempted slot. liveProductionAllowed must
// pin the startup head and refuse every slot at or below it (those a prior
// process could have broadcast) while allowing slots first reached live.
func TestLiveProductionAllowed_SkipsReplayedSlotsAfterRestart(t *testing.T) {
	bp := &BlockProducer{}

	// First tick after restart: head observed = 1000, captured as the floor.
	// A replayed slot (<= startupHead) must not produce.
	if bp.liveProductionAllowed(950, 1000) {
		t.Fatal("replayed slot below startupHead must not produce")
	}
	if bp.startupHead != 1000 {
		t.Fatalf("startupHead: got %d want 1000", bp.startupHead)
	}
	// A slot exactly at the floor could also have been attempted pre-crash.
	if bp.liveProductionAllowed(1000, 1100) {
		t.Fatal("slot == startupHead must not produce")
	}
	// First slot beyond the floor is provably first-seen this run.
	if !bp.liveProductionAllowed(1001, 1100) {
		t.Fatal("slot beyond startupHead must produce")
	}
	// The floor stays pinned to the first observed head, not later heads.
	if bp.startupHead != 1000 {
		t.Fatalf("startupHead drifted: got %d want 1000", bp.startupHead)
	}
}

// TestLiveProductionAllowed_WaitsForValidHead ensures we never produce (and
// never pin a bogus floor) before a real chain head is known.
func TestLiveProductionAllowed_WaitsForValidHead(t *testing.T) {
	bp := &BlockProducer{}
	if bp.liveProductionAllowed(500, 0) {
		t.Fatal("must not produce before a valid head is observed")
	}
	if bp.startupHead != 0 {
		t.Fatalf("startupHead must stay unset until head is valid, got %d", bp.startupHead)
	}
	// Once a valid head arrives it pins, and later slots produce.
	if bp.liveProductionAllowed(500, 600) {
		t.Fatal("slot below newly-pinned head must not produce")
	}
	if !bp.liveProductionAllowed(601, 600) {
		t.Fatal("slot above pinned head must produce")
	}
}
