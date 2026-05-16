package rc_system_test

import (
	"math"
	"math/big"
	"testing"

	"vsc-node/modules/common/params"
	rc_system "vsc-node/modules/rc-system"
)

// review2 MEDIUM #107 — CalculateFrozenBal was
// int64(diff * uint64(initialBal) / RC_RETURN_PERIOD): uint64(negative)
// wrapped huge, diff*uint64 overflowed, and a start>end underflow made diff
// huge → clamped to "fully returned" (frozen 0), under-charging RC.
//
// Differential: the underflow and overflow cases below produce the WRONG
// value on the #170 baseline (RED) and the correct value on fix/review2
// (GREEN). The normal/period/negative cases are sanity (same on both).
func TestReview2CalculateFrozenBal(t *testing.T) {
	period := params.RC_RETURN_PERIOD

	// sanity (identical old vs new for the valid domain)
	if got := rc_system.CalculateFrozenBal(0, 100, 1_000_000); got != 1_000_000-1_000_000*100/int64(period) {
		t.Fatalf("normal case: got %d", got)
	}
	if got := rc_system.CalculateFrozenBal(0, period, 1000); got != 0 {
		t.Fatalf("diff>=period: got %d, want 0", got)
	}

	// #107 negative balance → never a wrapped/negative frozen amount
	if got := rc_system.CalculateFrozenBal(0, 100, -1000); got != 0 {
		t.Fatalf("review2 #107: negative initialBal → got %d, want 0 (uint64(neg) wrap)", got)
	}

	// #107 start > end (node briefly behind / clock back): conservative —
	// fully frozen, NOT 0. Baseline underflowed diff → frozen 0 (under-charge).
	if got := rc_system.CalculateFrozenBal(50, 10, 1000); got != 1000 {
		t.Fatalf("review2 #107: start>end → got %d, want 1000 (baseline underflows to 0)", got)
	}

	// #107 overflow: diff * uint64(MaxInt64) overflows uint64 on baseline →
	// wrong amtRet. Correct = initialBal - initialBal*diff/period (big math).
	bal := int64(math.MaxInt64)
	exp := new(big.Int).Mul(big.NewInt(bal), big.NewInt(3))
	exp.Div(exp, new(big.Int).SetUint64(period))
	wantFrozen := bal - exp.Int64()
	got := rc_system.CalculateFrozenBal(0, 3, bal)
	if got != wantFrozen {
		t.Fatalf("review2 #107: overflow case got %d, want %d (baseline wraps uint64)", got, wantFrozen)
	}
	if got < 0 || got > bal {
		t.Fatalf("review2 #107: frozen %d out of [0,%d]", got, bal)
	}
}
