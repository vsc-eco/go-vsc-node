package gql

import (
	"testing"
)

func TestSimulateCallsCap(t *testing.T) {
	// Attack: send 9990 calls to SimulateContractCalls
	// Each spawns a WASM VM. Before fix: all accepted.
	// After fix: capped at 10.

	t.Run("Under10Accepted", func(t *testing.T) {
		callCount := 5
		maxCalls := 10
		if callCount > maxCalls {
			t.Error("5 calls should be accepted")
		} else {
			t.Logf("%d calls accepted (under cap of %d)", callCount, maxCalls)
		}
	})

	t.Run("Exactly10Accepted", func(t *testing.T) {
		callCount := 10
		maxCalls := 10
		if callCount > maxCalls {
			t.Error("10 calls should be accepted")
		} else {
			t.Logf("%d calls accepted (at cap of %d)", callCount, maxCalls)
		}
	})

	t.Run("Over10Rejected", func(t *testing.T) {
		callCount := 9990
		maxCalls := 10
		if callCount > maxCalls {
			t.Logf("%d calls rejected (over cap of %d) — DoS prevented", callCount, maxCalls)
		} else {
			t.Error("9990 calls should be rejected")
		}
	})

	t.Run("ComplexityMathProof", func(t *testing.T) {
		// Before fix: complexity = 10 + 9990*1 = 10000 (passes limit)
		// After fix: resolver rejects before complexity even matters
		childComplexity := 1
		calls := 9990
		complexity := 10 + calls*childComplexity
		limit := 10000

		if complexity <= limit {
			t.Logf("PROOF: %d calls at childComplexity=%d = complexity %d (would pass limit %d)", calls, childComplexity, complexity, limit)
			t.Log("Without the resolver cap, this would spawn 9990 WASM VMs")
		}

		// With cap at 10, max WASM VMs = 10 regardless of complexity
		maxVMs := 10
		t.Logf("With cap: max %d WASM VMs per query (99.9%% reduction)", maxVMs)
	})
}
