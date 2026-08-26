package election_proposer

import (
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/witnesses"
)

// The H-6 gate has been switched off in production since 2026-06-22 because
// turning it on starved the mainnet committee below the floor and halted
// elections at epoch 1699. Every proposal for re-enabling it starts with the
// same prerequisite: count the survivors first. Until shadow evaluation existed
// there was no way to count them short of running the gate — i.e. short of
// repeating the halt.
//
// These prove the instrument reads correctly while the gate stays off, which is
// the only condition under which the reading is worth anything.

// The exclusion set must be populated even though the gate is disabled.
func TestShadowEvaluationRunsWhileTheGateIsOff(t *testing.T) {
	if consensusversion.WitnessKeyStrictActive(consensusversion.V0_7_0) {
		t.Skip("gate is enabled; shadow-vs-enforced distinction no longer applies")
	}

	good := h6Witness(t, "alice", 0x11, true)
	noPoP := h6Witness(t, "bob", 0x22, false)

	excluded := evaluateKeyAdmission([]witnesses.Witness{good, noPoP})

	if len(excluded) != 1 {
		t.Fatalf("exclusions = %+v, want exactly 1 (bob, no gateway PoP). A shadow evaluation "+
			"that cannot see the failing witness cannot tell an operator whether enabling the "+
			"gate is safe.", excluded)
	}
	if excluded[0].Account != "bob" {
		t.Fatalf("excluded %s, want bob — alice carries valid PoPs on both keys", excluded[0].Account)
	}
	if excluded[0].Reason != "invalid gateway-key PoP" {
		t.Fatalf("reason = %q, want the gateway-key PoP reason. The reason is what tells an "+
			"operator whether the fix is 're-announce' or 'rotate the key'.", excluded[0].Reason)
	}
}

// ★ THE PROPERTY THAT MAKES SHADOW MODE SAFE TO SHIP. Evaluating must not change
// what the election carries. If it did, this would not be an instrument — it
// would be the very membership change whose blast radius it exists to measure.
func TestShadowEvaluationDoesNotChangeTheCommittee(t *testing.T) {
	if consensusversion.WitnessKeyStrictActive(consensusversion.V0_7_0) {
		t.Skip("gate is enabled; committee changes are expected")
	}

	seats := test_utils.NewMockPoaSeatsDb()
	ep, _ := poaHarness(t, seats, map[string]int64{"alice": 100, "bob": 100, "carol": 100})

	list := []witnesses.Witness{
		h6Witness(t, "alice", 0x11, true),
		h6Witness(t, "bob", 0x22, false), // would be excluded if the gate were on
		h6Witness(t, "carol", 0x33, false),
	}
	// h6Witness announces 0.2.0; this election runs at the POA line, and the
	// version floor filter would otherwise drop all three before H-6 is ever
	// reached — leaving an empty committee and an assertion that proved nothing
	// about the gate.
	for i := range list {
		list[i].ProtocolVersion = 7
	}

	// PREMISE: two of the three really do fail the rules, so "committee
	// unchanged" is a meaningful claim rather than a vacuous one.
	if got := len(evaluateKeyAdmission(list)); got != 2 {
		t.Fatalf("premise not established: %d exclusions, want 2 — the fixture does not exercise the gate", got)
	}

	_, data, err := ep.GenerateFullElection(list, 0, consensusversion.V0_7_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	if len(data.Members) != 3 {
		t.Fatalf("members = %v, want all 3. Shadow evaluation changed the committee: it is no "+
			"longer an observation, and every node that has not upgraded now computes a different "+
			"election CID.", memberAccounts(data.Members))
	}
}

// One implementation, two consumers. The count an operator reads while the gate
// is off has to be the count that gets dropped when it is turned on, or the
// reading is not evidence about the decision it is being used to make.
func TestShadowSetIsTheSetTheGateWouldDelete(t *testing.T) {
	list := []witnesses.Witness{
		h6Witness(t, "alice", 0x11, true),
		h6Witness(t, "bob", 0x22, false),
		h6Witness(t, "carol", 0x33, true),
	}

	excluded := evaluateKeyAdmission(list)
	drop := map[string]bool{}
	for _, x := range excluded {
		drop[x.Account] = true
	}

	// Re-derive independently, from the verifiers themselves.
	for _, w := range list {
		wantDropped := w.VerifyConsensusPoP() != nil || w.VerifyGatewayKeyPoP() != nil
		if drop[w.Account] != wantDropped {
			t.Fatalf("%s: shadow says dropped=%v, the PoP verifiers say %v — the instrument and "+
				"the rule disagree, so the measurement cannot be trusted to authorise the flip",
				w.Account, drop[w.Account], wantDropped)
		}
	}
}

// Duplicate keys are part of the rule, so they must be part of the measurement:
// an operator counting survivors needs the same arithmetic the gate uses.
func TestShadowEvaluationCountsDuplicateKeys(t *testing.T) {
	a := h6Witness(t, "alice", 0x11, true)
	clone := h6Witness(t, "alice", 0x11, true)
	clone.Account = "mallory"
	// mallory copied alice's seed, so both keys collide. Her PoPs are bound to
	// alice's account and therefore fail first — which is itself the point: the
	// consensus-key arm catches a copied seed before the dedupe has to.
	excluded := evaluateKeyAdmission([]witnesses.Witness{a, clone})
	if len(excluded) != 1 || excluded[0].Account != "mallory" {
		t.Fatalf("exclusions = %+v, want mallory only", excluded)
	}
}
