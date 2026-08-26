package election_proposer

import (
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/witnesses"
)

// These close the gap between "the seat gate is WIRED" and "the seat gate
// APPLIED". Every POA rule used to resolve through `poaSeats != nil`, which
// answers the first question, while the gate itself has two reachable paths on
// which it declines to filter anything. On both of them the old predicate still
// said the POA rules were in force, so candidacy was permissionless (anyone with
// MinStake) while weight was flat — committee weight purchasable at MinStake per
// seat, which is cheaper than the stake-weighting POA replaced and free of the
// vetting POA adds. TestPoaRulesApplyAsASetOrNotAtAll covers the third path
// (no registry wired at all); these cover the two the old predicate missed.

// DECLINE PATH 1: registry wired but EMPTY. The gate logs and proceeds ungated
// (TestPoaSeatGateInertWhenRegistryEmpty proves the members are unfiltered).
// The weights must be unfiltered too.
func TestEmptyRegistryDoesNotFlattenWeight(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb() // wired, empty
	ep, _ := poaHarness(t, seats, map[string]int64{
		"alice": 1_000_000, "bob": 100, "carol": 100,
	})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}

	// PREMISE: the gate really did decline, i.e. every candidate survived.
	// Without this the weight assertion below could pass because the committee
	// was filtered rather than because the rules stayed off.
	if len(data.Members) != 3 {
		t.Fatalf("premise not established: members = %v, want all 3 (an empty registry must leave candidacy ungated)",
			memberAccounts(data.Members))
	}

	var alice uint64
	for i, m := range data.Members {
		if m.Account == "alice" {
			alice = data.Weights[i]
		}
	}
	if alice == params.PoaSeatWeight {
		t.Fatal("weight was flattened while the seat gate was inert on an EMPTY registry. " +
			"Candidacy is permissionless on this path, so a flat weight means committee weight " +
			"costs MinStake per seat — strictly worse than either regime the gate sits between.")
	}
	if alice != 1_000_000 {
		t.Fatalf("alice weight = %d, want her raw stake 1000000 — POA rules must apply as a set or not at all", alice)
	}
}

// DECLINE PATH 2: registry wired and NON-EMPTY, but applying it would starve the
// committee below MinMembers, so the starvation guard refuses. Same reasoning:
// a refused gate must not leave the benefit half of POA switched on.
//
// This is the path a partial bootstrap produces, which is precisely why it must
// not be the path that makes committee weight cheap.
func TestStarvationRefusalDoesNotFlattenWeight(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	seats.Seed("alice", "ubo-a", 10) // one seat only: a partial bootstrap

	ep, _ := poaHarness(t, seats, map[string]int64{
		"alice": 100, "bob": 100, "carol": 100, "dave": 1_000_000,
	})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
		poaWitness(t, "dave", 0x44),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}

	// PREMISE: the starvation guard really did refuse. If the gate had applied,
	// only alice would remain and the weight check would be meaningless.
	if len(data.Members) != 4 {
		t.Fatalf("premise not established: members = %v (%d), want all 4 — the starvation guard did not refuse, so this test is not exercising the decline path",
			memberAccounts(data.Members), len(data.Members))
	}

	var dave uint64
	for i, m := range data.Members {
		if m.Account == "dave" {
			dave = data.Weights[i]
		}
	}
	if dave == params.PoaSeatWeight {
		t.Fatal("weight was flattened even though the seat gate REFUSED to apply. dave holds no " +
			"seat and was elected purely on stake, yet carries a full seat's weight: the gate's " +
			"refusal handed an unseated account the benefit of POA without its restriction.")
	}
	if dave != 1_000_000 {
		t.Fatalf("dave weight = %d, want his raw stake 1000000", dave)
	}
}

// The positive control. If the two tests above passed because flattening broke
// everywhere, this one fails — it asserts the rules DO apply when the gate
// genuinely filters.
func TestGateAppliedStillFlattensWeight(t *testing.T) {
	seats := test_utils.NewMockPoaSeatsDb()
	for _, a := range []string{"alice", "bob", "carol"} {
		seats.Seed(a, "ubo-"+a, 10)
	}
	ep, _ := poaHarness(t, seats, map[string]int64{
		"alice": 1_000_000, "bob": 100, "carol": 100, "mallory": 500_000,
	})
	list := []witnesses.Witness{
		poaWitness(t, "alice", 0x11),
		poaWitness(t, "bob", 0x22),
		poaWitness(t, "carol", 0x33),
		poaWitness(t, "mallory", 0x55),
	}
	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	if len(data.Members) != 3 {
		t.Fatalf("premise not established: members = %v, want the 3 seated accounts", memberAccounts(data.Members))
	}
	for i, w := range data.Weights {
		if w != params.PoaSeatWeight {
			t.Fatalf("weight[%d] (%s) = %d, want %d — the gate applied, so flat weight must apply with it",
				i, data.Members[i].Account, w, params.PoaSeatWeight)
		}
	}
}
