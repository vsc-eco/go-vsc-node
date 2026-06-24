package params

import "testing"

// TestSafetySlashActive_Schedules pins the windowed gate semantics the consensus
// boundary depends on: empty schedule is never active, a final open-ended window
// (End == 0) is active from its Start onward, closed windows are half-open
// [Start, End), and an on→off→on (reactivation) schedule is inactive in the gap.
// The gate must be a pure function of (schedule, height) so every node replays
// the identical slash/no-slash decision.
func TestSafetySlashActive_Schedules(t *testing.T) {
	cases := []struct {
		name    string
		windows []HeightWindow
		checks  map[uint64]bool // height -> expected active
	}{
		{
			name:    "empty is never active",
			windows: nil,
			checks:  map[uint64]bool{0: false, 1: false, 1_000_000: false},
		},
		{
			name:    "single open-ended window active from Start onward",
			windows: []HeightWindow{{Start: 100}},
			checks:  map[uint64]bool{0: false, 99: false, 100: true, 101: true, 1_000_000: true},
		},
		{
			name:    "single closed window is half-open [Start,End)",
			windows: []HeightWindow{{Start: 100, End: 200}},
			checks:  map[uint64]bool{99: false, 100: true, 150: true, 199: true, 200: false, 201: false},
		},
		{
			name:    "closed then open-ended with a gap (reactivation) is off in the gap",
			windows: []HeightWindow{{Start: 100, End: 200}, {Start: 300}},
			checks: map[uint64]bool{
				100: true, 199: true, // first window
				200: false, 250: false, 299: false, // gap: slashing disabled
				300: true, 9_999_999: true, // reactivated, open-ended
			},
		},
		{
			name: "mainnet-shaped: activate, emergency-disable, reactivate",
			windows: []HeightWindow{
				{Start: 107454300, End: 109_000_000},
				{Start: 120_000_000},
			},
			checks: map[uint64]bool{
				107454299:   false,
				107454300:   true,
				108_999_999: true,
				109_000_000: false, // disabled at H
				115_000_000: false, // still off in the gap
				120_000_000: true,  // reactivated
				200_000_000: true,
			},
		},
		{
			name:    "two closed windows, off after the last",
			windows: []HeightWindow{{Start: 10, End: 20}, {Start: 30, End: 40}},
			checks:  map[uint64]bool{15: true, 25: false, 35: true, 40: false, 50: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := ConsensusParams{SafetySlashWindows: tc.windows}
			// A valid-by-construction schedule must pass the validator too.
			if err := cp.ValidSafetySlashWindows(); err != nil {
				t.Fatalf("schedule should be valid: %v", err)
			}
			for h, want := range tc.checks {
				if got := cp.SafetySlashActive(h); got != want {
					t.Errorf("SafetySlashActive(%d) = %v, want %v", h, got, want)
				}
			}
		})
	}
}

// TestValidSafetySlashWindows covers the well-formedness invariant: sorted,
// disjoint, non-empty half-open intervals with only the final window open-ended.
// A malformed schedule is replay-divergent, so it must be rejected here (and is
// asserted against every shipped config in the systemconfig package).
func TestValidSafetySlashWindows(t *testing.T) {
	valid := []struct {
		name    string
		windows []HeightWindow
	}{
		{"empty", nil},
		{"single open", []HeightWindow{{Start: 1}}},
		{"single closed", []HeightWindow{{Start: 100, End: 200}}},
		{"closed then open with gap", []HeightWindow{{Start: 100, End: 200}, {Start: 300}}},
		{"closed then closed with gap", []HeightWindow{{Start: 100, End: 200}, {Start: 300, End: 400}}},
		{"touching back-to-back windows", []HeightWindow{{Start: 100, End: 200}, {Start: 200, End: 300}}},
		{"all closed, ends off", []HeightWindow{{Start: 10, End: 20}, {Start: 30, End: 40}}},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			cp := ConsensusParams{SafetySlashWindows: tc.windows}
			if err := cp.ValidSafetySlashWindows(); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []struct {
		name    string
		windows []HeightWindow
	}{
		{"mid-list open-ended", []HeightWindow{{Start: 100}, {Start: 200, End: 300}}},
		{"two open-ended", []HeightWindow{{Start: 100}, {Start: 200}}},
		{"empty interval End==Start", []HeightWindow{{Start: 100, End: 100}}},
		{"inverted End<Start", []HeightWindow{{Start: 200, End: 100}}},
		{"overlap", []HeightWindow{{Start: 100, End: 250}, {Start: 200, End: 300}}},
		{"overlap into open-ended", []HeightWindow{{Start: 100, End: 250}, {Start: 200}}},
		{"out of order", []HeightWindow{{Start: 300, End: 400}, {Start: 100, End: 200}}},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			cp := ConsensusParams{SafetySlashWindows: tc.windows}
			if err := cp.ValidSafetySlashWindows(); err == nil {
				t.Errorf("expected error for malformed schedule %v, got nil", tc.windows)
			}
		})
	}
}
