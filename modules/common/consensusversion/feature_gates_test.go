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

	// v0.3.0 features stay active at/above the later 0.4.0 line (MeetsConsensusMin
	// is monotone), so a 0.4.0-running binary still honors the governance batch.
	if !GovernanceActionsActive(V0_4_0) || !SafetySlashBurnDelay7dActive(V0_4_0) {
		t.Errorf("v0.3.0 gates must remain active at the 0.4.0 line")
	}
}

// TestV0_4_0Gates pins the v0.4.0 safety/correctness batch activation: every
// v0.4.0 feature is inert at/below 0.3.0 and active at/above 0.4.0, all keyed off
// the single V0_4_0 line so the GV4-3 election guard and the F14 unstake_hbd
// direction fix flip together. non_consensus is ignored.
func TestV0_4_0Gates(t *testing.T) {
	below := []Version{
		{Major: 0, Consensus: 0},
		{Major: 0, Consensus: 3},
		{Major: 0, Consensus: 3, NonConsensus: 9}, // 0.3.x is still below the line
	}
	atOrAbove := []Version{
		{Major: 0, Consensus: 4},
		{Major: 0, Consensus: 4, NonConsensus: 7}, // non_consensus ignored
		{Major: 0, Consensus: 9},
	}

	for _, v := range below {
		if Version0_4_0Active(v) {
			t.Errorf("Version0_4_0Active(%s) = true, want false (below the line)", v.Format())
		}
		if MinMembersGuardActive(v) {
			t.Errorf("MinMembersGuardActive(%s) = true, want false", v.Format())
		}
		if UnstakeHbdDirectionFixActive(v) {
			t.Errorf("UnstakeHbdDirectionFixActive(%s) = true, want false", v.Format())
		}
	}
	for _, v := range atOrAbove {
		if !Version0_4_0Active(v) {
			t.Errorf("Version0_4_0Active(%s) = false, want true (at/above the line)", v.Format())
		}
		if !MinMembersGuardActive(v) {
			t.Errorf("MinMembersGuardActive(%s) = false, want true", v.Format())
		}
		if !UnstakeHbdDirectionFixActive(v) {
			t.Errorf("UnstakeHbdDirectionFixActive(%s) = false, want true", v.Format())
		}
	}

	// v0.4.0 features stay active at/above the later 0.5.0 line (MeetsConsensusMin
	// is monotone), so a 0.5.0-running binary still honors the safety batch.
	if !MinMembersGuardActive(V0_5_0) || !UnstakeHbdDirectionFixActive(V0_5_0) {
		t.Errorf("v0.4.0 gates must remain active at the 0.5.0 line")
	}
}

// TestV0_5_0Gates pins the v0.5.0 consensus-delegation batch activation: every
// v0.5.0 feature is inert at/below 0.4.0 and active at/above 0.5.0, keyed off the
// single V0_5_0 line. non_consensus is ignored.
func TestV0_5_0Gates(t *testing.T) {
	below := []Version{
		{Major: 0, Consensus: 0},
		{Major: 0, Consensus: 4},
		{Major: 0, Consensus: 4, NonConsensus: 9}, // 0.4.x is still below the line
	}
	atOrAbove := []Version{
		{Major: 0, Consensus: 5},
		{Major: 0, Consensus: 5, NonConsensus: 7}, // non_consensus ignored
		{Major: 0, Consensus: 9},
	}

	for _, v := range below {
		if Version0_5_0Active(v) {
			t.Errorf("Version0_5_0Active(%s) = true, want false (below the line)", v.Format())
		}
		if DelegatedStakeActive(v) {
			t.Errorf("DelegatedStakeActive(%s) = true, want false", v.Format())
		}
	}
	for _, v := range atOrAbove {
		if !Version0_5_0Active(v) {
			t.Errorf("Version0_5_0Active(%s) = false, want true (at/above the line)", v.Format())
		}
		if !DelegatedStakeActive(v) {
			t.Errorf("DelegatedStakeActive(%s) = false, want true", v.Format())
		}
	}

	// (RunningVersion pin now lives in TestV0_6_0Gates — the newest line.)
}

// TestV0_6_0Gates pins the v0.6.0 outbound-vault-protections batch: every v0.6.0
// gate is inert at/below 0.5.x and active at/above 0.6.0, keyed off the single
// V0_6_0 line. As the newest line it also pins that the shipped binary runs the
// version it implements (so the election floor can rise to it).
func TestV0_6_0Gates(t *testing.T) {
	below := []Version{
		{Major: 0, Consensus: 0},
		{Major: 0, Consensus: 5},
		{Major: 0, Consensus: 5, NonConsensus: 9}, // 0.5.x is still below the line
	}
	atOrAbove := []Version{
		{Major: 0, Consensus: 6},
		{Major: 0, Consensus: 6, NonConsensus: 7}, // non_consensus ignored
		{Major: 0, Consensus: 9},
	}
	for _, v := range below {
		if Version0_6_0Active(v) || OutboundHaltActive(v) || OutboundDelayActive(v) {
			t.Errorf("a v0.6.0 gate is active at %s, want inert (below the line)", v.Format())
		}
	}
	for _, v := range atOrAbove {
		if !Version0_6_0Active(v) || !OutboundHaltActive(v) || !OutboundDelayActive(v) {
			t.Errorf("a v0.6.0 gate is inert at %s, want active (at/above the line)", v.Format())
		}
	}
	// The shipped binary must run the version it implements, so the floor can rise.
	if RunningVersion().Cmp(V0_6_0) != 0 {
		t.Errorf("RunningVersion() = %s, want %s (bump in the same commit as the v0.6.0 gates)",
			RunningVersion().Format(), V0_6_0.Format())
	}
}
