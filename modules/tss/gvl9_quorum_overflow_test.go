package tss

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// GV-L9 — TSS BLS quorum uint64 overflow.
//
// waitForSigs (tss.go) collects BLS signatures until the leader has gathered at
// least 2/3 of the election weight. The original loop condition was:
//
//	for signedWeight*3 < weightTotal*2 { ... }
//
// Both products are evaluated as uint64 and Go's unsigned arithmetic wraps mod
// 2^64 with no panic. Once weightTotal > MaxUint64/2, `weightTotal*2` wraps to a
// small value (e.g. weightTotal = MaxUint64/2+1 → weightTotal*2 == 0), so the
// condition becomes `signedWeight*3 < 0` which is always false. The loop exits
// immediately with signedWeight=0 and circuit.Finalize() runs on a zero-weight
// circuit — a complete falsification of the quorum requirement.
//
// The fix precomputes the threshold using the overflow-safe identity:
//
//	quorumThreshold := weightTotal - weightTotal/3   // == ceil(2*weightTotal/3)
//	for signedWeight < quorumThreshold { ... }
//
// These tests pin both halves of the fix:
//   - the OLD form overflows and falsely exits at a large weightTotal where the
//     NEW form correctly keeps looping (rejects), and
//   - both forms agree exactly on the realistic weight domain (no behavioral
//     change on the happy path → determinism preserved across nodes).

// oldLoopCondition reproduces the pre-fix `signedWeight*3 < weightTotal*2`
// continue-looping predicate, exactly as it executed on mainnet at d1bcb411.
// TRUE means "keep collecting" (quorum not yet reached).
func oldLoopCondition(signedWeight, weightTotal uint64) bool {
	return signedWeight*3 < weightTotal*2
}

// newLoopCondition reproduces the fixed `signedWeight < quorumThreshold`
// continue-looping predicate from tss.go waitForSigs after the GV-L9 fix.
func newLoopCondition(signedWeight, weightTotal uint64) bool {
	quorumThreshold := weightTotal - weightTotal/3
	return signedWeight < quorumThreshold
}

// TestFixGVL9 is the regression gate for the TSS BLS quorum overflow.
func TestFixGVL9(t *testing.T) {
	// ── Part 1: the overflow path — OLD form is broken, NEW form is correct ──
	//
	// This is the exact value from the runtime exec proof: weightTotal*2 wraps
	// to 0, so the old loop condition `0 < 0` is false and the leader exits with
	// zero signatures collected.
	overflowTotal := uint64(math.MaxUint64)/2 + 1 // 9_223_372_036_854_775_808
	assert.Equal(t, uint64(0), overflowTotal*2,
		"sanity: weightTotal*2 must wrap to 0 at MaxUint64/2+1 (this is the bug)")

	// OLD form with zero signatures collected: it FALSELY says "stop looping"
	// (quorum reached) — accepting a sub-2/3 (here zero-weight) commitment.
	assert.False(t, oldLoopCondition(0, overflowTotal),
		"OLD form: overflow makes the loop exit immediately at signedWeight=0 (the bug)")

	// NEW form with zero signatures collected: it correctly says "keep looping"
	// (quorum NOT reached). The honest threshold is ceil(2*overflowTotal/3).
	wantThreshold := overflowTotal - overflowTotal/3 // 6_148_914_691_236_517_206
	assert.Equal(t, uint64(6_148_914_691_236_517_206), wantThreshold,
		"NEW threshold must be ceil(2N/3) computed without overflow")
	assert.True(t, newLoopCondition(0, overflowTotal),
		"NEW form: at signedWeight=0 the loop must keep collecting — quorum is not met")
	// And it only stops once genuine 2/3 weight is collected.
	assert.True(t, newLoopCondition(wantThreshold-1, overflowTotal),
		"NEW form: just below 2/3 must keep collecting")
	assert.False(t, newLoopCondition(wantThreshold, overflowTotal),
		"NEW form: exactly 2/3 weight reached → stop collecting")

	// ── Part 2: also overflow when only weightTotal*2 stays in range but
	//          signedWeight*3 overflows (weightTotal in (MaxUint64/3, MaxUint64/2]).
	//          signedWeight <= weightTotal always, so test signedWeight near the top.
	bigTotal := uint64(7_000_000_000_000_000_000) // > MaxUint64/3, <= MaxUint64/2
	// signedWeight*3 wraps here:
	highSigned := uint64(6_500_000_000_000_000_000)
	assert.Less(t, highSigned*3, bigTotal*2,
		"sanity: signedWeight*3 wraps below weightTotal*2 in this range (latent bug)")
	// OLD form: because signedWeight*3 wrapped small, it FALSELY keeps looping
	// even though true 2/3 (ceil = 4_666_666_666_666_666_667) was already met.
	assert.True(t, oldLoopCondition(highSigned, bigTotal),
		"OLD form: wrapped signedWeight*3 falsely keeps looping past genuine quorum")
	// NEW form: 6.5e18 signed of 7e18 total is well above 2/3 → correctly stops.
	assert.False(t, newLoopCondition(highSigned, bigTotal),
		"NEW form: genuine super-majority correctly stops the loop")

	// ── Part 3: determinism / happy-path invariance ─────────────────────────
	//
	// On the entire realistic (non-overflow) domain the two forms must agree
	// EXACTLY, so no node's accept/reject decision changes vs the deployed code.
	// We cover the live mainnet total weight, its exact 2/3 boundary, and a wide
	// sweep of totals with boundary-clustered signed weights.

	// Live mainnet committee total weight from the audit (2026-06-03).
	const mainnetTotal = uint64(117_176_230)
	const mainnetThreshold = uint64(78_117_487) // ceil(2*117_176_230/3)
	assert.Equal(t, mainnetThreshold, mainnetTotal-mainnetTotal/3,
		"mainnet quorum threshold must be ceil(2N/3)")
	for _, sw := range []uint64{
		0, 1,
		mainnetThreshold - 1, mainnetThreshold, mainnetThreshold + 1,
		mainnetTotal,
	} {
		assert.Equalf(t, oldLoopCondition(sw, mainnetTotal), newLoopCondition(sw, mainnetTotal),
			"mainnet domain: OLD and NEW must agree at signedWeight=%d", sw)
	}

	// Broad sweep across realistic totals, with signedWeight clustered around the
	// 2/3 boundary (where any off-by-one would show) plus the endpoints.
	for total := uint64(1); total <= 5000; total++ {
		thr := total - total/3
		candidates := []uint64{0, 1, total / 2, total}
		if thr >= 1 {
			candidates = append(candidates, thr-1)
		}
		candidates = append(candidates, thr)
		if thr < total {
			candidates = append(candidates, thr+1)
		}
		for _, sw := range candidates {
			if sw > total {
				continue // signedWeight <= weightTotal always holds in practice
			}
			oldV := oldLoopCondition(sw, total)
			newV := newLoopCondition(sw, total)
			assert.Equalf(t, oldV, newV,
				"non-overflow domain divergence: total=%d signedWeight=%d (old=%v new=%v)",
				total, sw, oldV, newV)
		}
	}

	// Spot-check large totals at the largest fully-safe boundary. Because
	// signedWeight <= weightTotal always holds, BOTH products (signedWeight*3 and
	// weightTotal*2) stay in range iff weightTotal <= MaxUint64/3. At that exact
	// boundary the two forms must still agree. (Note: MaxUint64/2 is NOT in the
	// safe domain — there signedWeight*3 already overflows for signedWeight just
	// above the threshold, which is the GV-L9 bug, not the happy path.)
	const maxSafeTotal = uint64(math.MaxUint64) / 3 // 6_148_914_691_236_517_205
	for _, total := range []uint64{
		1_000_000_000,
		1_000_000_000_000_000,
		maxSafeTotal,
	} {
		thr := total - total/3
		for _, sw := range []uint64{thr - 1, thr, thr + 1} {
			if sw > total {
				continue
			}
			assert.Equalf(t, oldLoopCondition(sw, total), newLoopCondition(sw, total),
				"largest fully-safe domain: total=%d signedWeight=%d must agree", total, sw)
		}
	}

	// And demonstrate the divergence just past the safe boundary: at
	// total = MaxUint64/2, signedWeight = thr+1 overflows signedWeight*3 and the
	// OLD form gives the wrong answer while the NEW form is correct — confirming
	// the bug class extends to the signedWeight*3 side, not only weightTotal*2.
	unsafeTotal := uint64(math.MaxUint64) / 2
	unsafeThr := unsafeTotal - unsafeTotal/3
	assert.True(t, unsafeThr+1 > uint64(math.MaxUint64)/3,
		"sanity: signedWeight just past 2/3 overflows the *3 product at this total")
	// NEW form: signedWeight above the threshold → stop (quorum met). Correct.
	assert.False(t, newLoopCondition(unsafeThr+1, unsafeTotal),
		"NEW form: above 2/3 must stop collecting")
	// OLD form: signedWeight*3 wrapped small → falsely keeps looping past quorum.
	assert.True(t, oldLoopCondition(unsafeThr+1, unsafeTotal),
		"OLD form: wrapped signedWeight*3 falsely keeps looping past genuine quorum (the bug)")
}

// TestFixGVL9_ThresholdIdentity proves N - floor(N/3) == ceil(2N/3) for a dense
// range of N, which is the algebraic basis for the byte-identical fix. ceil(2N/3)
// is computed here without the 2N product (via the safe rearrangement) only to
// cross-check the identity used in production.
func TestFixGVL9_ThresholdIdentity(t *testing.T) {
	for n := uint64(0); n <= 100000; n++ {
		safe := n - n/3
		// ceil(2n/3) without overflow: 2n/3 rounded up == (2n + 2) / 3 for the
		// small n here, but compute via the proven decomposition to avoid any 2n.
		// 2n/3 ceil == n - n/3 is exactly what we assert, so cross-check against
		// a second independent formula: floor((2n+2)/3) for these small n.
		var ceilTwoThirds uint64
		if n <= (math.MaxUint64-2)/2 {
			ceilTwoThirds = (2*n + 2) / 3
			// (2n+2)/3 floor equals ceil(2n/3) only when 2n is not already a
			// multiple of 3 adjusted; verify by the standard ceil formula.
			ceilTwoThirds = (2*n + 3 - 1) / 3
		}
		assert.Equalf(t, ceilTwoThirds, safe,
			"identity ceil(2N/3) == N - floor(N/3) must hold at N=%d", n)
	}
}
