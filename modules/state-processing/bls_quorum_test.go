package state_engine_test

import (
	"testing"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/elections"
	state_engine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// review2 CRITICAL #6 — BLS quorum bypass. The state-engine accepted any
// cryptographically-valid aggregate, never checking that the signed weight
// reached 2/3 of total election weight (the rule the leader enforces in
// tss.go waitForSigs: signedWeight*3 >= weightTotal*2). These pin the
// quorum predicate, including the exact reported scenario: 3/6 equal-weight
// signers (below the 2/3 threshold of 4) must be rejected.

func mkMembers(n int) ([]elections.ElectionMember, []dids.BlsDID) {
	m := make([]elections.ElectionMember, n)
	d := make([]dids.BlsDID, n)
	for i := 0; i < n; i++ {
		key := "did:key:member-" + string(rune('A'+i))
		m[i] = elections.ElectionMember{Key: key, Account: "acct-" + string(rune('A'+i))}
		d[i] = dids.BlsDID(key)
	}
	return m, d
}

func equalWeights(n int) []uint64 {
	w := make([]uint64, n)
	for i := range w {
		w[i] = 1
	}
	return w
}

func TestBlsQuorumMet_EqualWeights_ReportedScenario(t *testing.T) {
	members, dd := mkMembers(6)
	w := equalWeights(6)

	// 3 of 6 — exactly the reported epochs 444-486 case. 3*3=9 < 6*2=12.
	assert.False(t, state_engine.BlsQuorumMet(dd[:3], members, w),
		"3/6 equal-weight signers is below 2/3 and must be rejected")

	// 4 of 6 — ceil(2/3*6)=4. 4*3=12 >= 12.
	assert.True(t, state_engine.BlsQuorumMet(dd[:4], members, w),
		"4/6 meets the 2/3 quorum")

	// All 6.
	assert.True(t, state_engine.BlsQuorumMet(dd, members, w))
}

func TestBlsQuorumMet_WeightedMembers(t *testing.T) {
	members, dd := mkMembers(3)
	w := []uint64{5, 3, 2} // total 10; need signedWeight*3 >= 20 → >= 7 (ceil)

	assert.False(t, state_engine.BlsQuorumMet(dd[1:], members, w), // 3+2=5 → 15 < 20
		"weight 5 of 10 is below 2/3")
	assert.True(t, state_engine.BlsQuorumMet(dd[:1], members, w) == false) // 5 → 15 < 20
	assert.True(t, state_engine.BlsQuorumMet([]dids.BlsDID{dd[0], dd[2]}, members, w), // 5+2=7 → 21 >= 20
		"weight 7 of 10 meets 2/3")
}

func TestBlsQuorumMet_FailsClosedOnBadInput(t *testing.T) {
	members, dd := mkMembers(3)

	assert.False(t, state_engine.BlsQuorumMet(dd, members, []uint64{1, 1}),
		"members/weights length mismatch must fail closed")
	assert.False(t, state_engine.BlsQuorumMet(dd, nil, nil),
		"empty election must fail closed")
	assert.False(t, state_engine.BlsQuorumMet(dd, members, []uint64{0, 0, 0}),
		"zero total weight must fail closed")
}

func TestBlsQuorumMet_UnknownAndDuplicateDIDsIgnored(t *testing.T) {
	members, dd := mkMembers(6)
	w := equalWeights(6)

	// A forged/duplicated bitset must not be able to inflate signed weight.
	stuffed := []dids.BlsDID{dd[0], dd[0], dd[0], "did:key:not-a-member"}
	assert.False(t, state_engine.BlsQuorumMet(stuffed, members, w),
		"duplicate + unknown DIDs must not reach quorum")
}
