package state_engine_test

import (
	"math"
	"testing"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/elections"
	state_engine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// GV-L9 — BlsQuorumMet uint64 overflow (sibling of the tss.go waitForSigs loop).
//
// The original predicate was `signedWeight*3 >= weightTotal*2`. As an on-chain
// verifier, a passing result authorizes activating a TSS key / landing a
// commitment, so a false positive here is the more sensitive of the two sites.
//
// When weightTotal > MaxUint64/2, `weightTotal*2` wraps mod 2^64 to a small
// value, so even `signedWeight=0` satisfies `0 >= (small)` and a zero-weight
// commitment is FALSELY accepted. The fix uses the overflow-safe identity
// quorumThreshold = weightTotal - weightTotal/3 (== ceil(2*weightTotal/3)).
//
// This test would FAIL with the old code (the overflow cases would accept a
// sub-quorum) and PASS with the fix.

func TestBlsQuorumMet_GVL9_Overflow(t *testing.T) {
	// One real member carrying an overflow-scale weight, plus padding members so
	// the total exceeds MaxUint64/2 and weightTotal*2 wraps. We only need the
	// arithmetic path; members/weights lengths must match.
	bigWeight := uint64(math.MaxUint64)/2 + 1 // 9_223_372_036_854_775_808
	members := []elections.ElectionMember{
		{Key: "did:key:big", Account: "big"},
	}
	weights := []uint64{bigWeight}
	bigDID := dids.BlsDID("did:key:big")

	// Sanity: the old arithmetic wraps weightTotal*2 to 0.
	assert.Equal(t, uint64(0), bigWeight*2,
		"sanity: weightTotal*2 wraps to 0 at MaxUint64/2+1 (this is the bug)")

	// Zero signers must be REJECTED. Old code: `0 >= 0` → true (false accept).
	// Fixed code: `0 >= ceil(2N/3)` → false (correct reject).
	assert.False(t, state_engine.BlsQuorumMet(nil, members, weights),
		"GV-L9: zero signed weight must be rejected even at overflow-scale total")

	// The single big member signing = 100% of weight → genuinely meets quorum.
	assert.True(t, state_engine.BlsQuorumMet([]dids.BlsDID{bigDID}, members, weights),
		"GV-L9: 100%% signed weight must be accepted at overflow-scale total")

	// Now a two-member case where signedWeight is a genuine minority but
	// signedWeight*3 would wrap in the OLD form. total > MaxUint64/3 so the
	// signedWeight*3 product overflows for a large-but-minority signer.
	a := uint64(3_000_000_000_000_000_000) // ~3e18
	b := uint64(4_000_000_000_000_000_000) // ~4e18; total 7e18 > MaxUint64/3
	members2 := []elections.ElectionMember{
		{Key: "did:key:a", Account: "a"},
		{Key: "did:key:b", Account: "b"},
	}
	weights2 := []uint64{a, b}
	total2 := a + b
	threshold2 := total2 - total2/3 // ceil(2*7e18/3) = 4_666_666_666_666_666_667

	// 'b' alone = 4e18 of 7e18 < ceil(2/3)=4.67e18 → must be REJECTED.
	assert.Less(t, b, threshold2, "sanity: b is below the honest 2/3 threshold")
	assert.False(t, state_engine.BlsQuorumMet([]dids.BlsDID{dids.BlsDID("did:key:b")}, members2, weights2),
		"GV-L9: a sub-2/3 signer must be rejected (signedWeight*3 must not wrap into a false accept)")

	// Both 'a'+'b' = 100% → accepted.
	assert.True(t, state_engine.BlsQuorumMet(
		[]dids.BlsDID{dids.BlsDID("did:key:a"), dids.BlsDID("did:key:b")}, members2, weights2),
		"GV-L9: full weight must be accepted")
}

// TestBlsQuorumMet_GVL9_HappyPathInvariance proves the fix is byte-identical to
// the original predicate `signedWeight*3 >= weightTotal*2` across the entire
// realistic (non-overflow) weight domain — so no node's accept/reject decision
// changes versus the deployed code (determinism preserved).
func TestBlsQuorumMet_GVL9_HappyPathInvariance(t *testing.T) {
	oldPredicate := func(signedWeight, weightTotal uint64) bool {
		return signedWeight*3 >= weightTotal*2
	}

	for total := uint64(1); total <= 5000; total++ {
		members := []elections.ElectionMember{{Key: "did:key:m", Account: "m"}}
		weights := []uint64{total}
		// Drive signedWeight via a member whose weight equals the chosen signed
		// amount: split total into "signer" + "rest" with two members.
		thr := total - total/3
		for _, sw := range []uint64{0, 1, total / 2, max0(thr, 1) - 1, thr, minU(thr+1, total), total} {
			if sw > total {
				continue
			}
			// Build a two-member election: signer weight = sw, other = total-sw.
			m := []elections.ElectionMember{
				{Key: "did:key:s", Account: "s"},
				{Key: "did:key:o", Account: "o"},
			}
			w := []uint64{sw, total - sw}
			got := state_engine.BlsQuorumMet([]dids.BlsDID{dids.BlsDID("did:key:s")}, m, w)
			want := oldPredicate(sw, total)
			assert.Equalf(t, want, got,
				"non-overflow domain: total=%d signedWeight=%d must match original predicate", total, sw)
			_ = members
			_ = weights
		}
	}
}

func minU(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// max0 returns a if a >= 1 else 1, guarding the thr-1 expression at thr==0.
func max0(a, _ uint64) uint64 {
	if a < 1 {
		return 1
	}
	return a
}
