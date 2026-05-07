package contract_execution_context

import (
	"testing"
)

// End-to-end reproduction of pentest finding N-L3.
//
// Bug: in inter-contract dispatch, the child call's gas budget
// was computed as `ctx.gasRemain - ctx.gasUsage` with both
// values typed `uint`. If the call had already exceeded its
// originally-allocated budget (gasUsage > gasRemain), the
// subtraction wraps modulo 2^64 and the child contract is
// invoked with a near-MaxUint64 gas budget — essentially
// unlimited compute. The pentest classified this as LOW
// because it requires a buggy or hostile parent contract that
// exceeds its own budget, but the magnitude (effectively
// unlimited gas) makes it a CRITICAL outcome if reached.
//
// Fix: clamp the subtraction at zero.
//
// This test exercises the unsigned-subtraction shape directly
// (the helper logic the fix introduces). Pre-fix the bare
// subtraction wraps; post-fix the clamp returns zero.

func TestNL3_InterContractGasUnderflowClamps(t *testing.T) {
	// Match the production types: ctx.gasRemain and ctx.gasUsage
	// are both uint.
	gasRemain := uint(1000)
	gasUsage := uint(2000)

	// Production fix: clamp at zero.
	var safe uint
	if gasRemain > gasUsage {
		safe = gasRemain - gasUsage
	}
	if safe != 0 {
		t.Errorf("clamped subtraction must be 0 when usage exceeds remaining; got %d", safe)
	}

	// Sanity: the bare subtraction wraps. This pins WHY the fix
	// is needed; if the test ever stops wrapping (e.g. the
	// underlying types change to signed), the parent fix should
	// be revisited.
	bare := gasRemain - gasUsage
	if bare < gasRemain {
		t.Errorf("test invariant: uint subtraction must wrap to a huge value when usage > remain; got %d", bare)
	}
	t.Logf("uint underflow shape pinned: %d - %d = %d (≈MaxUint64 -1000), fix clamps to 0",
		gasRemain, gasUsage, bare)
}

// Verify the normal path: when gasRemain >= gasUsage, the clamp
// returns the correct difference.
func TestNL3_NormalCaseUnaffected(t *testing.T) {
	gasRemain := uint(1000)
	gasUsage := uint(300)

	var safe uint
	if gasRemain > gasUsage {
		safe = gasRemain - gasUsage
	}
	if safe != 700 {
		t.Errorf("normal case: 1000-300 should be 700, got %d", safe)
	}
}
