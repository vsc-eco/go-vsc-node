package systemconfig

import "testing"

// TestShippedSafetySlashWindowsValid asserts every network's compiled-in safety
// slash schedule is well-formed. Because the slash debit is replay-derived, a
// malformed (overlapping / out-of-order / mid-list open-ended) schedule would
// diverge nodes on reindex — so a bad constant must fail the build here, not the
// chain at runtime.
func TestShippedSafetySlashWindowsValid(t *testing.T) {
	configs := map[string]SystemConfig{
		"mainnet": MainnetConfig(),
		"testnet": TestnetConfig(),
		"devnet":  DevnetConfig(),
		"mocknet": MocknetConfig(),
	}
	for name, conf := range configs {
		t.Run(name, func(t *testing.T) {
			if err := conf.ConsensusParams().ValidSafetySlashWindows(); err != nil {
				t.Fatalf("%s safety slash schedule is malformed: %v", name, err)
			}
		})
	}
}

// TestShippedSafetySlashFrozenActivation pins each network's IMMUTABLE first
// activation height — the block at which safety slashing first went live. That
// boundary is settled history (live nodes already ran it), so it must never
// move; this fails the build if an edit changes it. The UPPER bound of a window
// (an emergency-disable height, or a later reactivation Start) is intentionally
// MUTABLE and is deliberately NOT pinned here — TestShippedSafetySlashWindowsBoundaries
// validates whatever schedule is shipped instead.
func TestShippedSafetySlashFrozenActivation(t *testing.T) {
	frozen := map[string]struct {
		conf  SystemConfig
		start uint64 // 0 == inert: schedule must be empty
	}{
		"mainnet": {MainnetConfig(), 107454300},
		"testnet": {TestnetConfig(), 3_870_000},
		"devnet":  {DevnetConfig(), 1},
		"mocknet": {MocknetConfig(), 0},
	}
	for name, f := range frozen {
		t.Run(name, func(t *testing.T) {
			ws := f.conf.ConsensusParams().SafetySlashWindows
			if f.start == 0 {
				if len(ws) != 0 {
					t.Fatalf("%s: expected inert (no windows), got %v", name, ws)
				}
				return
			}
			if len(ws) == 0 {
				t.Fatalf("%s: expected first activation at frozen %d, got empty schedule", name, f.start)
			}
			if ws[0].Start != f.start {
				t.Fatalf("%s: first activation height moved (frozen history): got %d, want %d", name, ws[0].Start, f.start)
			}
		})
	}
}

// TestShippedSafetySlashWindowsBoundaries validates the on/off transitions of
// each network's ACTUAL shipped schedule against the gate, so the assertions
// track edits (closing a window at an emergency-disable height, appending a
// reactivation window) without baking in mutable magic heights. It still catches
// off-by-one / inverted gate logic at every real boundary.
func TestShippedSafetySlashWindowsBoundaries(t *testing.T) {
	for _, conf := range []SystemConfig{MainnetConfig(), TestnetConfig(), DevnetConfig(), MocknetConfig()} {
		cp := conf.ConsensusParams()
		for i, w := range cp.SafetySlashWindows {
			// Active at Start.
			if !cp.SafetySlashActive(w.Start) {
				t.Errorf("window %d {start:%d}: expected active at Start", i, w.Start)
			}
			// First window: inactive just before activation (frozen boundary).
			// Later windows' lower edge is covered by the prior window's "inactive
			// at End" check below, so we skip Start-1 there to avoid the
			// touching-window edge case.
			if i == 0 && w.Start > 0 && cp.SafetySlashActive(w.Start-1) {
				t.Errorf("window 0 {start:%d}: expected inactive at Start-1", w.Start)
			}
			// Closed window: active at End-1, inactive at End (the disable edge).
			if w.End != 0 {
				if !cp.SafetySlashActive(w.End - 1) {
					t.Errorf("window %d: expected active at End-1 (%d)", i, w.End-1)
				}
				if cp.SafetySlashActive(w.End) {
					t.Errorf("window %d: expected inactive at End (%d)", i, w.End)
				}
			}
		}
	}
}
