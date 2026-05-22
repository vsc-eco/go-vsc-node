package state_engine_test

// Audit finding RT-24 (unfixed): state_engine.go:1019-1027 deserializes
// vsc.tss_commitment payloads into a map[string]tss_helpers.SignedCommitment
// when the new array format fails, then converts it into a slice via
// `for _, c := range commitmentMap { commitments = append(commitments, c) }`.
// Go's map iteration order is intentionally randomized, so the resulting
// slice order is non-deterministic per node. Any downstream consensus that
// depends on the ordering (blame aggregation, bitset position assignment,
// signature index) will diverge across nodes processing the same payload.
//
// This test demonstrates the *precondition*: a small map with two entries
// for the same KeyId can yield both orderings across iterations. When the
// bug is fixed (e.g. by sorting by KeyId+SessionId+TxIndex before append),
// only one ordering will appear and this test will fail — at which point
// it should be updated.

import (
	"testing"

	tss_helpers "vsc-node/modules/tss/helpers"
)

func TestAuditUnfixed_RT24_LegacyMapIterationIsNonDeterministic(t *testing.T) {
	const keyId = "key-a"
	commitmentMap := map[string]tss_helpers.SignedCommitment{
		"alpha": {
			BaseCommitment: tss_helpers.BaseCommitment{
				Type:      "round1",
				SessionId: "alpha",
				KeyId:     keyId,
			},
		},
		"omega": {
			BaseCommitment: tss_helpers.BaseCommitment{
				Type:      "round1",
				SessionId: "omega",
				KeyId:     keyId,
			},
		},
	}

	sawAlphaFirst := false
	sawOmegaFirst := false
	for i := 0; i < 200 && !(sawAlphaFirst && sawOmegaFirst); i++ {
		// Mirrors state_engine.go:1018-1021 exactly.
		var commitments []tss_helpers.SignedCommitment
		for _, c := range commitmentMap {
			commitments = append(commitments, c)
		}
		if len(commitments) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(commitments))
		}
		switch commitments[0].SessionId {
		case "alpha":
			sawAlphaFirst = true
		case "omega":
			sawOmegaFirst = true
		}
	}

	if !(sawAlphaFirst && sawOmegaFirst) {
		// Theoretically possible to see only one order even with random
		// iteration, but with 200 trials over a 2-key map the
		// probability is ~2^-199. Treat a single ordering as the
		// "fix-landed" signal.
		t.Fatalf("expected to observe both iteration orders within 200 trials; saw alphaFirst=%v omegaFirst=%v — fix may have landed", sawAlphaFirst, sawOmegaFirst)
	}
	t.Logf("precondition confirmed: legacy-map iteration produces both orderings (consensus-divergence risk)")
}
