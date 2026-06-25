package consensusversion

import "testing"

// TestV0_3_0Gates pins the v0.3.0 governance batch activation: every v0.3.0
// feature is inert at/below 0.2.0 and active at/above 0.3.0, all keyed off the
// single V0_3_0 line so the governance ops and the 7-day pending window flip
// together. non_consensus is ignored (coordination is on major.consensus only).
func TestV0_3_0Gates(t *testing.T) {
	below := []Version{
		{Major: 0, Consensus: 0},
		{Major: 0, Consensus: 1},
		{Major: 0, Consensus: 2, NonConsensus: 9}, // 0.2.x is still below the line
	}
	// Coordination is componentwise on major.consensus (MeetsConsensusMin), so
	// "above" means same major with consensus >= 3 (a major bump resets consensus
	// and is a separate coordination — not exercised here).
	atOrAbove := []Version{
		{Major: 0, Consensus: 3},
		{Major: 0, Consensus: 3, NonConsensus: 7}, // non_consensus ignored
		{Major: 0, Consensus: 4},
		{Major: 0, Consensus: 9},
	}

	for _, v := range below {
		if Version0_3_0Active(v) {
			t.Errorf("Version0_3_0Active(%s) = true, want false (below the line)", v.Format())
		}
		if GovernanceActionsActive(v) {
			t.Errorf("GovernanceActionsActive(%s) = true, want false", v.Format())
		}
		if SafetySlashBurnDelay7dActive(v) {
			t.Errorf("SafetySlashBurnDelay7dActive(%s) = true, want false", v.Format())
		}
	}
	for _, v := range atOrAbove {
		if !Version0_3_0Active(v) {
			t.Errorf("Version0_3_0Active(%s) = false, want true (at/above the line)", v.Format())
		}
		if !GovernanceActionsActive(v) {
			t.Errorf("GovernanceActionsActive(%s) = false, want true", v.Format())
		}
		if !SafetySlashBurnDelay7dActive(v) {
			t.Errorf("SafetySlashBurnDelay7dActive(%s) = false, want true", v.Format())
		}
	}

	// The shipped binary must run the version it implements, so the floor can rise.
	if RunningVersion().Cmp(V0_3_0) != 0 {
		t.Errorf("RunningVersion() = %s, want %s (bump in the same commit as the v0.3.0 gates)",
			RunningVersion().Format(), V0_3_0.Format())
	}
}
