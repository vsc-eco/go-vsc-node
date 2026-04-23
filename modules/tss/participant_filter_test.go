package tss

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
)

// accountsOf extracts account names from a []Participant for readable
// assertions.
func accountsOf(ps []Participant) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Account)
	}
	return out
}

func TestBuildSignParticipants_DeterministicFromInputs(t *testing.T) {
	// Same inputs must produce byte-identical output. This is the
	// consensus-critical property: any divergence here causes
	// SortedPartyIDs to differ across nodes, producing different
	// Paillier key mappings in BuildLocalSaveDataSubset and failing
	// round 2 with "failed to calculate Bob_mid or Bob_mid_wc".
	election := makeElection(371, []string{"alice", "bob", "carol", "dave", "eve"})
	bitset := new(big.Int).SetInt64(0b11111)
	blamed := map[string]bool{"bob": true, "carol": true}
	banned := map[string]bool{}

	listA, sizeA := buildSignParticipants(election, bitset, blamed, banned)
	listB, sizeB := buildSignParticipants(election, bitset, blamed, banned)

	assert.Equal(t, accountsOf(listA), accountsOf(listB),
		"identical inputs must produce identical participant lists")
	assert.Equal(t, sizeA, sizeB,
		"identical inputs must produce identical fullSize")
	assert.Equal(t, []string{"alice", "dave", "eve"}, accountsOf(listA),
		"blamed members excluded; bitset-set members kept in election order")
	assert.Equal(t, 5, sizeA, "fullSize is pre-filter bitset count")
}

func TestBuildSignParticipants_ExcludesBlamedAndBanned(t *testing.T) {
	election := makeElection(1, []string{"alice", "bob", "carol", "dave", "eve"})
	bitset := new(big.Int).SetInt64(0b11111)

	cases := []struct {
		name    string
		blamed  map[string]bool
		banned  map[string]bool
		expect  []string
	}{
		{
			name:   "none excluded",
			blamed: map[string]bool{},
			banned: map[string]bool{},
			expect: []string{"alice", "bob", "carol", "dave", "eve"},
		},
		{
			name:   "blamed only",
			blamed: map[string]bool{"bob": true, "eve": true},
			banned: map[string]bool{},
			expect: []string{"alice", "carol", "dave"},
		},
		{
			name:   "banned only",
			blamed: map[string]bool{},
			banned: map[string]bool{"alice": true},
			expect: []string{"bob", "carol", "dave", "eve"},
		},
		{
			name:   "blamed + banned overlap",
			blamed: map[string]bool{"bob": true, "carol": true},
			banned: map[string]bool{"carol": true, "dave": true},
			expect: []string{"alice", "eve"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fullSize := buildSignParticipants(election, bitset, tc.blamed, tc.banned)
			assert.Equal(t, tc.expect, accountsOf(got))
			assert.Equal(t, 5, fullSize, "fullSize always reflects pre-filter bitset count")
		})
	}
}

func TestBuildSignParticipants_BitsetSkipsNonMembers(t *testing.T) {
	// Bitset 0b10101 selects indices 0, 2, 4 from the election members.
	election := makeElection(1, []string{"alice", "bob", "carol", "dave", "eve"})
	bitset := new(big.Int).SetInt64(0b10101)

	got, fullSize := buildSignParticipants(election, bitset, nil, nil)
	assert.Equal(t, []string{"alice", "carol", "eve"}, accountsOf(got))
	assert.Equal(t, 3, fullSize, "fullSize reflects only bits set in the bitset")
}

func TestBuildSignParticipants_EmptyBitset(t *testing.T) {
	// When the commitment isn't available, bv stays as a zero big.Int. The
	// helper must handle this by returning an empty list.
	election := makeElection(1, []string{"alice", "bob"})
	bitset := big.NewInt(0)

	got, fullSize := buildSignParticipants(election, bitset,
		map[string]bool{}, map[string]bool{})
	assert.Empty(t, got)
	assert.Equal(t, 0, fullSize)
}

func TestBuildReshareParticipants_Deterministic(t *testing.T) {
	commitElection := makeElection(369, []string{"a", "b", "c", "d", "e"})
	currentElection := makeElection(371, []string{"a", "b", "c", "d", "f"})
	bitset := new(big.Int).SetInt64(0b11111)
	blamed := map[string]bool{"b": true}
	banned := map[string]bool{"e": true}

	oldA, newA, sizeA := buildReshareParticipants(
		commitElection, bitset, currentElection, blamed, banned)
	oldB, newB, sizeB := buildReshareParticipants(
		commitElection, bitset, currentElection, blamed, banned)

	assert.Equal(t, accountsOf(oldA), accountsOf(oldB))
	assert.Equal(t, accountsOf(newA), accountsOf(newB))
	assert.Equal(t, sizeA, sizeB)

	// Old committee = (a,b,c,d,e) ∩ bitset − blamed(b) − banned(e) = a,c,d
	assert.Equal(t, []string{"a", "c", "d"}, accountsOf(oldA))
	// New committee = (a,b,c,d,f) − blamed(b) − banned(e) = a,c,d,f
	assert.Equal(t, []string{"a", "c", "d", "f"}, accountsOf(newA))
	// oldFullSize = pre-filter bitset count = 5
	assert.Equal(t, 5, sizeA)
}

func TestBuildReshareParticipants_OldFullSizeUnaffectedByFilter(t *testing.T) {
	// oldFullSize must always reflect the pre-filter bitset count — it
	// feeds into threshold math that must match the keygen polynomial
	// degree. Filtering blamed/banned must not change this number.
	commitElection := makeElection(10, []string{"a", "b", "c", "d", "e"})
	currentElection := makeElection(11, []string{"a", "b", "c", "d", "e"})
	bitset := new(big.Int).SetInt64(0b11111)

	cases := []struct {
		name   string
		blamed map[string]bool
		banned map[string]bool
	}{
		{"none", nil, nil},
		{"one blamed", map[string]bool{"a": true}, nil},
		{"three banned", nil, map[string]bool{"a": true, "b": true, "c": true}},
		{"all blamed", map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, oldFullSize := buildReshareParticipants(
				commitElection, bitset, currentElection, tc.blamed, tc.banned)
			assert.Equal(t, 5, oldFullSize,
				"oldFullSize must always reflect pre-filter bitset count")
		})
	}
}

func TestBuildReshareParticipants_BitsetBounds(t *testing.T) {
	// Indices beyond bitset.BitLen() must not contribute. Verify the
	// guard against stale commitments where the election grew after
	// the commitment was made.
	commitElection := makeElection(1, []string{"a", "b", "c", "d", "e"})
	currentElection := makeElection(2, []string{"a", "b", "c", "d", "e"})
	bitset := new(big.Int).SetInt64(0b00101) // only indices 0 and 2

	old, _, oldFullSize := buildReshareParticipants(
		commitElection, bitset, currentElection,
		map[string]bool{}, map[string]bool{})
	assert.Equal(t, []string{"a", "c"}, accountsOf(old))
	assert.Equal(t, 2, oldFullSize)
}

// This test is the direct regression guard for the bug report. It
// reproduces the 5-node testnet scenario where two nodes had different
// gossip readiness views but identical on-chain state. With the fix,
// the participant list no longer depends on gossip, so divergent views
// cannot produce divergent lists.
func TestBuildSignParticipants_RegressionBobMidBugReport(t *testing.T) {
	// Testnet election: 5 members.
	election := makeElection(371, []string{
		"magi.test1", "magi.test2", "magi.test3", "milo.magi", "tibfox",
	})
	// Commitment bitset had all 5 members in the keygen committee.
	bitset := new(big.Int).SetInt64(0b11111)
	// Two nodes got blamed from prior reshare timeouts.
	blamed := map[string]bool{"magi.test2": true, "magi.test3": true}
	banned := map[string]bool{}

	// Node A (magi.test2): had all 5 gossip attestations.
	// Node B (magi.test1): was missing tibfox's attestation.
	// Pre-fix: node A produced {m1, milo, tibfox, m2, m3} (fallback fired, 5)
	//          node B produced {m1, milo, m2, m3} (fallback fired, 4, no tibfox)
	// Post-fix: gossip is NOT a parameter — both produce the same list.
	nodeA, _ := buildSignParticipants(election, bitset, blamed, banned)
	nodeB, _ := buildSignParticipants(election, bitset, blamed, banned)

	assert.Equal(t, accountsOf(nodeA), accountsOf(nodeB),
		"two nodes with identical on-chain state must produce identical lists")
	assert.Equal(t, []string{"magi.test1", "milo.magi", "tibfox"}, accountsOf(nodeA),
		"blamed excluded; bitset-set members kept in election order")
}
