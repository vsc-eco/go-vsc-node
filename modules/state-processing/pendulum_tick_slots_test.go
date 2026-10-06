package state_engine

import "testing"

// ELECT-50: below 0.9.0 the window is unchanged (the slot ending AT the tick is
// scored, and it is also the first slot of the next tick); from 0.9.0 only slots
// that closed before the tick are scored, each in exactly one tick.
func TestTickSlotRange(t *testing.T) {
	const slot = 10
	// Old window, byte for byte: 11 slots, the boundary slot included.
	if f, l := tickSlotRange(7140400, 7140500, slot, false); f != 7140400 || l != 7140500 {
		t.Fatalf("pre-0.9 window changed: %d..%d", f, l)
	}
	// New window: the slot ending at 7140500 (its block lands after 7140500) waits
	// for the next tick.
	if f, l := tickSlotRange(7140400, 7140500, slot, true); f != 7140400 || l != 7140490 {
		t.Fatalf("closed-slots window: %d..%d, want 7140400..7140490", f, l)
	}
	// Tiny heights stay empty rather than underflow.
	if f, l := tickSlotRange(0, 5, slot, true); l >= f {
		t.Fatalf("tick below one slot must score nothing, got %d..%d", f, l)
	}
}

// Consecutive ticks under the new window cover every slot exactly once.
func TestTickSlotRangeTilesWithoutOverlapOrGap(t *testing.T) {
	const slot, window = 10, 100
	seen := map[uint64]int{}
	for tick := uint64(7140000); tick <= 7145000; tick += window {
		f, l := tickSlotRange(tick-window, tick, slot, true)
		for s := f; s <= l; s += slot {
			seen[s]++
		}
	}
	for s := uint64(7140000 - window); s <= 7145000-slot; s += slot {
		if seen[s] > 1 {
			t.Fatalf("slot %d scored %d times", s, seen[s])
		}
		if s >= 7140000 && seen[s] != 1 {
			t.Fatalf("slot %d scored %d times, want once", s, seen[s])
		}
	}
	// The old window double-counts the boundary slot of every pair of ticks.
	old := map[uint64]int{}
	for tick := uint64(7140100); tick <= 7140200; tick += window {
		f, l := tickSlotRange(tick-window, tick, slot, false)
		for s := f; s <= l; s += slot {
			old[s]++
		}
	}
	if old[7140100] != 2 {
		t.Fatalf("expected the old window to score boundary slot 7140100 twice, got %d", old[7140100])
	}
}
