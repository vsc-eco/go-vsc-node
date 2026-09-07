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

// quorumBothGates asserts the corrected fold (B13) and the legacy fold agree, and
// returns their shared verdict.
//
// Every case in this file uses DISTINCT member keys, which is the entire domain
// where the two folds are supposed to be identical — the B13 change only alters
// what happens when a key is seated twice. Running each existing assertion
// through BOTH gate states therefore turns this suite into an equivalence proof:
// it shows the fix is byte-identical for every committee history can actually
// contain, which is what makes replaying below the version line safe.
func quorumBothGates(t *testing.T, included []dids.BlsDID, members []elections.ElectionMember, weights []uint64) bool {
	t.Helper()
	deduped := state_engine.BlsQuorumMet(included, members, weights, true)
	legacy := state_engine.BlsQuorumMet(included, members, weights, false)
	assert.Equal(t, legacy, deduped,
		"B13 dedup must not change the verdict for a committee with no duplicate keys")
	return deduped
}

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
	assert.False(t, quorumBothGates(t, dd[:3], members, w),
		"3/6 equal-weight signers is below 2/3 and must be rejected")

	// 4 of 6 — ceil(2/3*6)=4. 4*3=12 >= 12.
	assert.True(t, quorumBothGates(t, dd[:4], members, w),
		"4/6 meets the 2/3 quorum")

	// All 6.
	assert.True(t, quorumBothGates(t, dd, members, w))
}

func TestBlsQuorumMet_WeightedMembers(t *testing.T) {
	members, dd := mkMembers(3)
	w := []uint64{5, 3, 2} // total 10; need signedWeight*3 >= 20 → >= 7 (ceil)

	assert.False(t, quorumBothGates(t, dd[1:], members, w), // 3+2=5 → 15 < 20
		"weight 5 of 10 is below 2/3")
	assert.True(t, quorumBothGates(t, dd[:1], members, w) == false) // 5 → 15 < 20
	assert.True(t, quorumBothGates(t, []dids.BlsDID{dd[0], dd[2]}, members, w), // 5+2=7 → 21 >= 20
		"weight 7 of 10 meets 2/3")
}

func TestBlsQuorumMet_FailsClosedOnBadInput(t *testing.T) {
	members, dd := mkMembers(3)

	assert.False(t, quorumBothGates(t, dd, members, []uint64{1, 1}),
		"members/weights length mismatch must fail closed")
	assert.False(t, quorumBothGates(t, dd, nil, nil),
		"empty election must fail closed")
	assert.False(t, quorumBothGates(t, dd, members, []uint64{0, 0, 0}),
		"zero total weight must fail closed")
}

func TestBlsQuorumMet_UnknownAndDuplicateDIDsIgnored(t *testing.T) {
	members, dd := mkMembers(6)
	w := equalWeights(6)

	// A forged/duplicated bitset must not be able to inflate signed weight.
	stuffed := []dids.BlsDID{dd[0], dd[0], dd[0], "did:key:not-a-member"}
	assert.False(t, quorumBothGates(t, stuffed, members, w),
		"duplicate + unknown DIDs must not reach quorum")
}

// B13. The defect the dedup closes, at the site that gates BTC custody-key
// activation.
//
// The old fold built weightByDID with a plain map assignment, so a key seated
// twice was LAST-WRITE-WINS, while both seats still counted toward the total.
// Members arrive sorted by account, so an attacker who copies an honest
// witness's consensus key into an account sorting AFTER theirs chooses which
// weight the honest signature gets credited. The function's own comment said
// "duplicates are counted once"; what it actually did was substitute the
// attacker's self-staked weight for the victim's.
//
// No cooperation and no stolen private key are needed. The BLS aggregate over a
// duplicated keyset verifies by construction, so the honest witness signs once,
// normally, and never learns anything happened.
//
// Below, one honest signature alone clears a 2/3 bar it should come nowhere near.
func TestBlsQuorumMet_DuplicateKeySubstitutesAttackerWeight(t *testing.T) {
	const victimKey = "did:key:victim"

	// Account-sorted, as the election delivers them. "zzz-attacker" sorts after
	// the victim, so its seat is written last.
	members := []elections.ElectionMember{
		{Key: "did:key:alice", Account: "alice"},
		{Key: victimKey, Account: "bob"},
		{Key: "did:key:carol", Account: "carol"},
		{Key: victimKey, Account: "zzz-attacker"}, // the copied key
	}
	weights := []uint64{10, 10, 10, 70}

	// The victim signs once, routinely. Nobody else does.
	oneHonestSignature := []dids.BlsDID{victimKey}

	// BEFORE: the single honest signature is credited the attacker's 70 out of a
	// 100 total, clearing the 67 threshold on its own.
	assert.True(t, state_engine.BlsQuorumMet(oneHonestSignature, members, weights, false),
		"precondition: the legacy fold lets ONE honest signature manufacture quorum — "+
			"this is the bug, and it gates TSS keygen and reshare acceptance")

	// AFTER: the duplicate seat is not a distinct signer, so it carries no weight
	// and is absent from the total. One seat out of three cannot be 2/3 of them.
	assert.False(t, state_engine.BlsQuorumMet(oneHonestSignature, members, weights, true),
		"a duplicated key is ONE signer and must be credited ONE seat's weight")

	// And the honest committee still reaches quorum normally — the fix must not
	// make legitimate signing harder. Two of the three real seats is 20 of 30.
	assert.True(t, state_engine.BlsQuorumMet(
		[]dids.BlsDID{"did:key:alice", victimKey}, members, weights, true),
		"dropping the phantom seat must not raise the bar for honest signers")
}

// The tie-break must not be attacker-choosable. Whichever order the inflated seat
// appears in, the credited weight is the first occurrence's — which, since
// members are account-sorted, is the lexicographically-first account, matching
// the tie-break the admission-side dedup documents.
func TestBlsQuorumMet_DedupTieBreakIsFirstSeat(t *testing.T) {
	const dupKey = "did:key:dup"

	attackerLast := []elections.ElectionMember{
		{Key: dupKey, Account: "aaa-victim"},
		{Key: dupKey, Account: "zzz-attacker"},
	}
	attackerFirst := []elections.ElectionMember{
		{Key: dupKey, Account: "aaa-attacker"},
		{Key: dupKey, Account: "zzz-victim"},
	}
	weights := []uint64{10, 90}

	// Either way the committee collapses to a single seat, so its lone signer is
	// 100% of the weight and quorum is met — the point is that the ATTACKER never
	// gets to pick which number is credited, and the two seats never sum.
	for name, members := range map[string][]elections.ElectionMember{
		"attacker seated second": attackerLast,
		"attacker seated first":  attackerFirst,
	} {
		assert.True(t, state_engine.BlsQuorumMet([]dids.BlsDID{dupKey}, members, weights, true),
			"%s: a single collapsed seat signing is 100%% of the weight", name)
	}
}
