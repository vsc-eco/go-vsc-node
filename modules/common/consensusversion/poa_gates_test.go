package consensusversion

import "testing"

// The POA batch is a MEMBERSHIP change — the highest-consequence shape in this
// codebase (the structurally identical H-6 key-admission gate halted mainnet
// elections at epoch 1699 and is still disabled). These tests pin the two
// properties that make the batch safe to ship dark: every POA gate is inert
// below 0.7.0, and every POA gate flips together at 0.7.0. A gate that flipped
// on its own would activate half a membership rule set — e.g. a seat registry
// gate live while flat weights are not — which is a state no design reviewed.

func poaGates() map[string]func(Version) bool {
	return map[string]func(Version) bool{
		"PoaAdmissionOpsActive": PoaAdmissionOpsActive,
		"PoaSeatGateActive":     PoaSeatGateActive,
		"PoaFlatWeightActive":   PoaFlatWeightActive,
		"PoaChurnCapActive":     PoaChurnCapActive,
		"PoaExitHaltActive":     PoaExitHaltActive,
	}
}

func TestPoaGatesInertBelow0_7_0(t *testing.T) {
	below := []Version{
		{Major: 0, Consensus: 0, NonConsensus: 0},
		{Major: 0, Consensus: 1, NonConsensus: 0},
		{Major: 0, Consensus: 2, NonConsensus: 0},
		{Major: 0, Consensus: 3, NonConsensus: 0}, // the current mainnet floor
		{Major: 0, Consensus: 4, NonConsensus: 0}, // develop's delegated-stake line
		{Major: 0, Consensus: 5, NonConsensus: 0},
		{Major: 0, Consensus: 6, NonConsensus: 0}, // vault-protection's line
		// A high NON-consensus component must not drag the batch in: only the
		// consensus component gates consensus features.
		{Major: 0, Consensus: 6, NonConsensus: 99},
	}
	for name, gate := range poaGates() {
		for _, v := range below {
			if gate(v) {
				t.Fatalf("%s active at %+v — POA must be inert below 0.7.0", name, v)
			}
		}
	}
}

func TestPoaGatesActiveAtAndAbove0_7_0(t *testing.T) {
	atOrAbove := []Version{
		{Major: 0, Consensus: 7, NonConsensus: 0},
		{Major: 0, Consensus: 7, NonConsensus: 5},
		{Major: 0, Consensus: 8, NonConsensus: 0},
		{Major: 1, Consensus: 7, NonConsensus: 0},
	}
	for name, gate := range poaGates() {
		for _, v := range atOrAbove {
			if !gate(v) {
				t.Fatalf("%s inactive at %+v — POA must be in force at/above 0.7.0", name, v)
			}
		}
	}
}

// Every POA gate must agree with every other POA gate at EVERY version. This is
// the property that makes "one coordinated activation" true rather than
// aspirational; if someone later re-points one resolver at a different line,
// this fails rather than shipping a half-active membership rule set.
func TestPoaGatesFlipTogether(t *testing.T) {
	gates := poaGates()
	for c := uint64(0); c <= 12; c++ {
		v := Version{Major: 0, Consensus: c, NonConsensus: 0}
		want := Version0_7_0Active(v)
		for name, gate := range gates {
			if got := gate(v); got != want {
				t.Fatalf("%s(%+v) = %v, want %v — POA gates must flip as one batch",
					name, v, got, want)
			}
		}
	}
}

// MeetsConsensusMin is COMPONENTWISE (Major >= min.Major && Consensus >=
// min.Consensus, version.go:126-128), so the consensus counter does NOT reset
// across a major bump: 1.0.0 does not satisfy a 0.7.0 minimum. This is a
// pre-existing property of the whole version mechanism — V0_2_0 and V0_3_0
// behave identically — not something the POA batch introduces, and it is why
// the case above is 1.7.0 rather than 1.0.0. Pinned here so the behaviour is
// deliberate rather than discovered later by a failing gate.
func TestMajorBumpDoesNotResetConsensusComponent(t *testing.T) {
	if Version0_7_0Active(Version{Major: 1, Consensus: 0, NonConsensus: 0}) {
		t.Fatal("1.0.0 satisfied a 0.7.0 minimum — consensus comparison is no longer componentwise")
	}
	// Same shape for the existing batches, proving this is not POA-specific.
	if Version0_3_0Active(Version{Major: 1, Consensus: 0, NonConsensus: 0}) {
		t.Fatal("1.0.0 satisfied a 0.3.0 minimum — comparison semantics changed")
	}
}

// 0.7.0 must not collide with a line already claimed by another batch on
// another branch: a shared line means one floor rise silently activates two
// unrelated batches after a merge.
func TestPoaVersionLineIsDistinct(t *testing.T) {
	for _, taken := range []Version{V0_2_0, V0_3_0} {
		if V0_7_0 == taken {
			t.Fatalf("POA line %+v collides with an in-branch batch", V0_7_0)
		}
	}
	// 0.4.0/0.5.0 (origin/develop) and 0.6.0 (origin/feat/vault-protection) are
	// not declared on this branch, so assert on the numeric line directly.
	if V0_7_0.Consensus <= 6 {
		t.Fatalf("POA line consensus=%d must be >6: 4/5 are develop's delegated-stake batch and 6 is vault-protection's",
			V0_7_0.Consensus)
	}
}

// The shipped binary must announce the highest batch it implements, or the
// election floor can never rise to activate it: a witness whose announced
// version is below the pinned floor is deleted from the committee
// (election-proposer.go, PinnedVersionFloor filter), so a floor rise to 0.7.0
// while the binary still announces 0.3.0 would empty the committee. POA is the
// highest batch now, so RunningVersion must be 0.7.0. Bump currentConsensus in
// version.go in the SAME commit as any change here.
func TestRunningVersionImplementsPoa(t *testing.T) {
	if RunningVersion().Cmp(V0_7_0) != 0 {
		t.Errorf("RunningVersion() = %s, want %s (bump currentConsensus in version.go when shipping the POA batch)",
			RunningVersion().Format(), V0_7_0.Format())
	}
}
