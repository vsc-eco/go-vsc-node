package params

import "testing"

// TestSafetySlashBurnDelayConstants pins the 3d/7d pending-window constants. The
// 3d↔7d CHOICE is no longer a params method — it moved to a version gate
// (consensusversion.SafetySlashBurnDelay7dActive, resolved in state-processing
// from the chain-active version at the slash's own height), so the gate's
// behavior is covered there and in the consensusversion package. This guards only
// that the window magnitudes themselves don't drift.
func TestSafetySlashBurnDelayConstants(t *testing.T) {
	if SafetySlashBurnDelay3dBlocks != 3*28800 {
		t.Errorf("SafetySlashBurnDelay3dBlocks drifted: got %d, want %d", SafetySlashBurnDelay3dBlocks, 3*28800)
	}
	if SafetySlashBurnDelay7dBlocks != 7*28800 {
		t.Errorf("SafetySlashBurnDelay7dBlocks drifted: got %d, want %d", SafetySlashBurnDelay7dBlocks, 7*28800)
	}
}
