package params

import "testing"

// TestSafetySlashBurnDelayBlocks pins the forward-only 3d→7d gate. The window is
// a function of the SLASH's own height so maturity (slashHeight+delay) recomputes
// identically on replay and raising the window can't retroactively shift history.
func TestSafetySlashBurnDelayBlocks(t *testing.T) {
	const h = uint64(108_000_000)

	// Not scheduled (0): always the 3-day window.
	unset := ConsensusParams{SafetySlashBurnDelay7dHeight: 0}
	for _, height := range []uint64{0, h, h + 1_000_000} {
		if got := unset.SafetySlashBurnDelayBlocks(height); got != SafetySlashBurnDelay3dBlocks {
			t.Errorf("unset gate at height %d = %d, want 3d (%d)", height, got, SafetySlashBurnDelay3dBlocks)
		}
	}

	// Scheduled at h: 3d strictly before, 7d at/after.
	cp := ConsensusParams{SafetySlashBurnDelay7dHeight: h}
	cases := map[uint64]uint64{
		h - 1: SafetySlashBurnDelay3dBlocks,
		h:     SafetySlashBurnDelay7dBlocks,
		h + 1: SafetySlashBurnDelay7dBlocks,
		0:     SafetySlashBurnDelay3dBlocks,
	}
	for height, want := range cases {
		if got := cp.SafetySlashBurnDelayBlocks(height); got != want {
			t.Errorf("SafetySlashBurnDelayBlocks(%d) = %d, want %d", height, got, want)
		}
	}

	// Sanity on the constants.
	if SafetySlashBurnDelay3dBlocks != 3*28800 || SafetySlashBurnDelay7dBlocks != 7*28800 {
		t.Fatalf("unexpected window constants: 3d=%d 7d=%d", SafetySlashBurnDelay3dBlocks, SafetySlashBurnDelay7dBlocks)
	}
}
