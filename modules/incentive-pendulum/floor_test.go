package pendulum

import "testing"

func TestMaxNodeShareBps(t *testing.T) {
	cases := []struct {
		name    string
		minFrac int64
		want    int64
	}{
		{"zero disables floor", 0, BpsScale},
		{"negative disables floor", -100, BpsScale},
		{"10% floor", 1_000, 9_000},
		{"50% floor", 5_000, 5_000},
		{"100% floor zeroes node ceiling", BpsScale, 0},
		{"above 100% clamped, no wrap", BpsScale + 5_000, 0},
	}
	for _, c := range cases {
		if got := MaxNodeShareBps(c.minFrac); got != c.want {
			t.Fatalf("%s: MaxNodeShareBps(%d) = %d, want %d", c.name, c.minFrac, got, c.want)
		}
	}
}

// The floor activates at consensus 0.2.0 — the source line bumped in the
// activating commit. Guards against the gate drifting away from the version.
func TestLPFloorActivationLine(t *testing.T) {
	if LPFloorActivation.Major != 0 || LPFloorActivation.Consensus != 2 {
		t.Fatalf("LPFloorActivation = %s, want 0.2.x", LPFloorActivation.Format())
	}
}

// The chosen default policy is a 75% node-share cap (25% LP floor). Pin it so a
// stray edit to the constant trips a test rather than silently changing the
// economic split.
func TestDefaultLPFloorCapsNodeAt75(t *testing.T) {
	if DefaultLPFloorBps != 2_500 {
		t.Fatalf("DefaultLPFloorBps = %d, want 2500 (25%% LP floor)", DefaultLPFloorBps)
	}
	if got := MaxNodeShareBps(DefaultLPFloorBps); got != 7_500 {
		t.Fatalf("default node-share cap = %d, want 7500 (75%%)", got)
	}
}
