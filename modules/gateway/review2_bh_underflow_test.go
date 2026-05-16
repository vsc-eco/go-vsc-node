package gateway

import (
	"testing"
)

// review2 LOW #112/#113 — two uint64 underflows on a fresh/low chain:
//
//	#112 multisig.go BlockTick:    `bh < *headHeight-20`
//	#113 multisig.go waitCheckBh:  `blockHeight < ms.bh-10`
//
// When the head/node height is below the constant, the `-20`/`-10`
// underflows to ~1.8e19, inverting the comparison: BlockTick skips every
// early block, and waitCheckBh rejects every early block as "too far into
// past". Both are fixed identically by the predicate-equivalent
// underflow-safe form (`x+N < y` instead of `x < y-N`).
//
// #113 is unit-observable on waitCheckBh's return value; #112 has the
// same one-line transformation in the same hunk (BlockTick's skip has no
// dependency-free observable). This test pins the #113 differential:
// "too far into past" on the #170 baseline (RED), nil on fix (GREEN).
func TestReview2WaitCheckBhNoUnderflow(t *testing.T) {
	// Node is at block 5 — below the `-10` constant.
	ms := &MultiSig{bh: 5}

	// blockHeight 0: 0 % INTERVAL == 0 (passes the interval gate), and
	// 0 <= ms.bh so we hit the `else if` past-check branch (no waiting).
	// fix:      0+10 < 5  -> false -> nil
	// baseline: 0 < 5-10(=huge) -> true -> "too far into past"
	if err := ms.waitCheckBh(20, 0); err != nil {
		t.Fatalf("review2 #113: waitCheckBh(20,0) with ms.bh=5 returned %q, want nil "+
			"(baseline underflows ms.bh-10 and wrongly rejects as 'too far into past')", err)
	}

	// Sanity: a genuinely too-old block (well behind a high head) is still
	// rejected — identical on both arms, so the guard is not weakened.
	msHigh := &MultiSig{bh: 1000}
	if err := msHigh.waitCheckBh(20, 100); err == nil || err.Error() != "too far into past" {
		t.Fatalf("review2 #113: waitCheckBh(20,100) with ms.bh=1000 = %v, want 'too far into past'", err)
	}
}
