package pendulumwasm

import (
	"testing"

	wasm_context "vsc-node/modules/wasm/context"
)

// TestAuditUnfixed_29_DustSwapBypassesCLPFee pins audit finding #29:
//
// For tiny x and large Y, the CLP base fee is computed as
//
//	baseCLP = floor(x² · Y / (x + X)²)
//
// using intmath.MulDivFloor (applier.go:168-178). With x small relative
// to X, this floors to zero — a fee of zero base units is "charged" and
// the dust swap bypasses CLP entirely. The audit recommendation is to
// floor baseCLP to MIN_CLP_BASE_UNIT (e.g. 1) so any swap that produces
// a positive grossOut also produces a positive CLP charge.
//
// Repro args: x=10, X=10_000_000, Y=10_000_000.
//
//	grossOut = floor(10 · 10_000_000 / 10_000_010) = 9
//	baseCLP  = floor(100 · 10_000_000 / 10_000_010²) = 0 ← bug
//	baseProt = floor(9 · 8 / 10000) = 0
//
// Both fee legs floor to zero, so the swap succeeds with zero accrual.
//
// Post-fix: when grossOut > 0, baseCLP should be max(baseCLP, MIN_CLP_BASE_UNIT)
// so the node bucket credit is strictly positive (or the swap is rejected
// as too small to charge).
func TestAuditUnfixed_29_DustSwapBypassesCLPFee(t *testing.T) {
	a, acc := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})

	// x=10 produces grossOut=9 but baseCLP = floor(10² · 10_000_000 /
	// 10_000_010²) = floor(1e9 / 1e14) = 0. Bug: zero CLP fee charged.
	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hive",
		AssetOut: "hbd",
		X:        10,
		XReserve: 10_000_000,
		YReserve: 10_000_000,
	}

	res := a.ApplySwapFees("contract:pool-1", "tx-dust-2", 100, args, acc.fn)
	if res.IsErr() {
		t.Fatalf("expected dust swap to succeed (current unfixed behavior), got err: %v", res)
	}
	out := res.Unwrap()

	// Bug signature: NodeBucketCreditedHBD is 0 because both base fee legs
	// floor to zero under the current MulDivFloor math.
	if out.NodeBucketCreditedHBD != 0 {
		t.Fatalf("unfixed-bug repro broken: expected zero node bucket credit (baseCLP floors to 0), got %d", out.NodeBucketCreditedHBD)
	}
	// User receives the full grossOut, no fees withheld — confirms the
	// audit finding directly.
	if out.UserOutput <= 0 {
		t.Fatalf("expected positive user output, got %d", out.UserOutput)
	}
	// Post-fix should floor to MIN_CLP_BASE_UNIT: NodeBucketCreditedHBD > 0
	// (or the swap should be rejected as too small to charge).
}

// TestAuditUnfixed_80_MultipleApplySwapFeesPerExecution pins audit finding
// #80: ApplySwapFees has no per-execution idempotency guard. Calling it
// twice with the same txID on the same Applier succeeds both times and
// the node bucket accrues twice. The audit fix is a per-execution
// `appliedOnce[txID]` flag (applier.go:298 area).
//
// Post-fix: the second call with an already-seen txID should return an
// error (e.g. errAlreadyApplied) and the accrual callback must be invoked
// only once.
func TestAuditUnfixed_80_MultipleApplySwapFeesPerExecution(t *testing.T) {
	a, acc := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hbd",
		AssetOut: "hive",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}

	res1 := a.ApplySwapFees("contract:pool-1", "tx-dup", 100, args, acc.fn)
	if res1.IsErr() {
		t.Fatalf("first call expected to succeed, got %v", res1)
	}
	out1 := res1.Unwrap()
	if out1.NodeBucketCreditedHBD <= 0 {
		t.Fatalf("expected positive accrual on first call, got %d", out1.NodeBucketCreditedHBD)
	}

	// Second call, SAME txID, on the SAME Applier instance.
	res2 := a.ApplySwapFees("contract:pool-1", "tx-dup", 100, args, acc.fn)
	if res2.IsErr() {
		// Post-fix: this is the desired branch. Today (unfixed) the bug
		// repro requires that the second call succeed; if a future fix
		// lands this test will start failing loudly, prompting the
		// reviewer to flip this branch to the post-fix assertions.
		t.Fatalf("unfixed-bug repro broken: expected second call with same txID to succeed (no idempotency guard), got %v", res2)
	}
	out2 := res2.Unwrap()
	if out2.NodeBucketCreditedHBD <= 0 {
		t.Fatalf("expected positive accrual on second call, got %d", out2.NodeBucketCreditedHBD)
	}

	// Both accrual calls landed → bucket got credited twice.
	if len(acc.calls) != 2 {
		t.Fatalf("expected 2 accrual calls (no idempotency guard), got %d", len(acc.calls))
	}
	// Same args + same geometry → identical accrual amount per call. Total
	// movement is 2× single-call value, double-spending the node bucket.
	if acc.calls[0] != acc.calls[1] {
		t.Fatalf("expected identical accrual amounts (deterministic), got %d vs %d", acc.calls[0], acc.calls[1])
	}
	total := acc.calls[0] + acc.calls[1]
	if total != 2*acc.calls[0] {
		t.Fatalf("total accrual %d != 2× single-call %d", total, 2*acc.calls[0])
	}
}
