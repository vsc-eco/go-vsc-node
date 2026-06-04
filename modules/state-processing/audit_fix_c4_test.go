package state_engine

import (
	"testing"

	"vsc-node/modules/db/vsc/elections"
)

// TestFixGVL11_PotentialWeightAccumulates — regression for GV-L11. Before the
// fix, the weighted branch ASSIGNED potentialWeight = int(Weights[idx]) on each
// iteration, leaving only the LAST member's weight. The fix accumulates (+=),
// extracted into aggregatePotentialWeight (the function ExecuteTx now calls).
//
// This drives the real helper. Under the buggy assignment it would return the
// last weight (10_000_000), not the sum (65_370_000), so this assertion fails
// without the fix.
func TestFixGVL11_PotentialWeightAccumulates(t *testing.T) {
	// Live-shape weighted committee (N=20), last weight 10_000_000.
	weights := []uint64{
		1_000_000, 2_000_000, 3_000_000, 4_000_000, 5_000_000,
		1_500_000, 2_500_000, 3_500_000, 4_500_000, 5_500_000,
		1_200_000, 2_200_000, 3_200_000, 4_200_000, 5_200_000,
		1_360_000, 2_360_000, 3_360_000, 4_360_000, 10_000_000,
	}
	members := make([]elections.ElectionMember, len(weights))
	var want int
	for i := range weights {
		members[i] = elections.ElectionMember{Account: "n", Key: "did:key:z6X"}
		want += int(weights[i])
	}
	// Sanity: the sum must exceed the last weight (otherwise the test cannot
	// distinguish accumulation from the last-weight assignment bug).
	if want <= int(weights[len(weights)-1]) {
		t.Fatalf("test setup: sum %d must exceed last weight %d", want, weights[len(weights)-1])
	}

	el := &elections.ElectionResult{}
	el.Members = members
	el.Weights = weights

	got := aggregatePotentialWeight(el)
	if got != want {
		t.Fatalf("GV-L11 regressed: aggregatePotentialWeight returned %d (last-weight bug?), want sum %d", got, want)
	}
	// The original last-weight bug would have produced exactly the final weight.
	if got == int(weights[len(weights)-1]) {
		t.Fatalf("GV-L11 regressed: result equals last member weight (%d), accumulation broken", got)
	}
}

// TestFixGVL11_UnweightedCountsMembers — the unweighted branch (no Weights) must
// count members, unchanged by the fix.
func TestFixGVL11_UnweightedCountsMembers(t *testing.T) {
	el := &elections.ElectionResult{}
	el.Members = make([]elections.ElectionMember, 7)
	// no Weights set → len(Weights)==0 branch
	if got := aggregatePotentialWeight(el); got != 7 {
		t.Fatalf("GV-L11: unweighted committee must count 7 members, got %d", got)
	}
}

// NOTE: the GV-L12 regression test lives in audit_fix_c4_gvl12_test.go
// (package state_engine_test). It must drive the REAL state_engine.go:1196
// staleness check through ProcessBlock, which requires the external-package
// TestEnv harness — so it cannot live in this internal-package file.
