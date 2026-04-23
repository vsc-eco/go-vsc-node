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

	listA, sizeA := buildSignParticipants(election, bitset, blamed, banned, nil)
	listB, sizeB := buildSignParticipants(election, bitset, blamed, banned, nil)

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
			got, fullSize := buildSignParticipants(election, bitset, tc.blamed, tc.banned, nil)
			assert.Equal(t, tc.expect, accountsOf(got))
			assert.Equal(t, 5, fullSize, "fullSize always reflects pre-filter bitset count")
		})
	}
}

func TestBuildSignParticipants_BitsetSkipsNonMembers(t *testing.T) {
	// Bitset 0b10101 selects indices 0, 2, 4 from the election members.
	election := makeElection(1, []string{"alice", "bob", "carol", "dave", "eve"})
	bitset := new(big.Int).SetInt64(0b10101)

	got, fullSize := buildSignParticipants(election, bitset, nil, nil, nil)
	assert.Equal(t, []string{"alice", "carol", "eve"}, accountsOf(got))
	assert.Equal(t, 3, fullSize, "fullSize reflects only bits set in the bitset")
}

func TestBuildSignParticipants_EmptyBitset(t *testing.T) {
	// When the commitment isn't available, bv stays as a zero big.Int. The
	// helper must handle this by returning an empty list.
	election := makeElection(1, []string{"alice", "bob"})
	bitset := big.NewInt(0)

	got, fullSize := buildSignParticipants(election, bitset,
		map[string]bool{}, map[string]bool{}, nil)
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
		commitElection, bitset, currentElection, blamed, banned, nil)
	oldB, newB, sizeB := buildReshareParticipants(
		commitElection, bitset, currentElection, blamed, banned, nil)

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
				commitElection, bitset, currentElection, tc.blamed, tc.banned, nil)
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
		map[string]bool{}, map[string]bool{}, nil)
	assert.Equal(t, []string{"a", "c"}, accountsOf(old))
	assert.Equal(t, 2, oldFullSize)
}

// Two nodes with identical on-chain state AND identical gossip snapshots
// must produce identical lists. This is the convergence assumption that the
// gossip-readiness filter relies on (CLAUDE.md Constraint 2). The long
// readiness window with repeated bundle re-broadcast drives convergence
// in practice.
func TestBuildSignParticipants_IdenticalOnChainAndGossipMatch(t *testing.T) {
	election := makeElection(371, []string{
		"magi.test1", "magi.test2", "magi.test3", "milo.magi", "tibfox",
	})
	bitset := new(big.Int).SetInt64(0b11111)
	blamed := map[string]bool{"magi.test2": true, "magi.test3": true}
	banned := map[string]bool{}
	gossip := map[string]bool{
		"magi.test1": true, "magi.test2": true, "magi.test3": true,
		"milo.magi": true, "tibfox": true,
	}

	nodeA, _ := buildSignParticipants(election, bitset, blamed, banned, gossip)
	nodeB, _ := buildSignParticipants(election, bitset, blamed, banned, gossip)

	assert.Equal(t, accountsOf(nodeA), accountsOf(nodeB))
	assert.Equal(t, []string{"magi.test1", "milo.magi", "tibfox"}, accountsOf(nodeA))
}

// Documents the divergence failure mode from CLAUDE.md Constraint 2: two
// honest nodes with different gossip snapshots produce different party lists.
// The runtime cost of this divergence is a silent SSID mismatch → timeout →
// blame commitment. This test does not assert divergence is acceptable; it
// exists so reviewers can see the worst case in code form.
func TestBuildSignParticipants_DivergentGossipDivergentLists(t *testing.T) {
	election := makeElection(371, []string{"alice", "bob", "carol", "dave", "eve"})
	bitset := new(big.Int).SetInt64(0b11111)

	nodeAGossip := map[string]bool{"alice": true, "bob": true, "carol": true, "dave": true, "eve": true}
	nodeBGossip := map[string]bool{"alice": true, "bob": true, "dave": true, "eve": true} // missing carol

	listA, _ := buildSignParticipants(election, bitset, nil, nil, nodeAGossip)
	listB, _ := buildSignParticipants(election, bitset, nil, nil, nodeBGossip)

	assert.Equal(t, []string{"alice", "bob", "carol", "dave", "eve"}, accountsOf(listA))
	assert.Equal(t, []string{"alice", "bob", "dave", "eve"}, accountsOf(listB))
	assert.NotEqual(t, accountsOf(listA), accountsOf(listB),
		"different gossip snapshots produce different lists — SSID mismatch risk")
}

func TestBuildSignParticipants_NilGossipDisablesFilter(t *testing.T) {
	// nil readyAccounts means no readiness filter, preserving the
	// strictly-on-chain participant list for callers/tests that don't care
	// about liveness.
	election := makeElection(1, []string{"alice", "bob", "carol"})
	bitset := new(big.Int).SetInt64(0b111)
	got, fullSize := buildSignParticipants(election, bitset,
		map[string]bool{}, map[string]bool{}, nil)
	assert.Equal(t, []string{"alice", "bob", "carol"}, accountsOf(got))
	assert.Equal(t, 3, fullSize)
}

func TestBuildSignParticipants_GossipFiltersOfflineNode(t *testing.T) {
	// The Phase 4 scenario: 6-node committee, node 5 offline → not in gossip.
	// Filter excludes offline node so btss CanProceed is not blocked on it.
	election := makeElection(1, []string{"n1", "n2", "n3", "n4", "n5", "n6"})
	bitset := new(big.Int).SetInt64(0b111111)
	gossip := map[string]bool{"n1": true, "n2": true, "n3": true, "n4": true, "n5": true} // n6 offline

	got, fullSize := buildSignParticipants(election, bitset, nil, nil, gossip)
	assert.Equal(t, []string{"n1", "n2", "n3", "n4", "n5"}, accountsOf(got))
	assert.Equal(t, 6, fullSize, "fullSize is the pre-filter bitset count, used for threshold math")
}

func TestBuildReshareParticipants_GossipFiltersBothCommittees(t *testing.T) {
	// Reshare requires every listed party in both committees to send messages.
	// Apply the readiness filter symmetrically so an offline node in either
	// committee is excluded from both.
	commitElection := makeElection(0, []string{"a", "b", "c", "d", "e"})
	currentElection := makeElection(1, []string{"a", "b", "c", "d", "f"})
	bitset := new(big.Int).SetInt64(0b11111)
	gossip := map[string]bool{"a": true, "c": true, "d": true, "f": true} // b and e offline

	old, new_, oldFullSize := buildReshareParticipants(
		commitElection, bitset, currentElection,
		nil, nil, gossip,
	)
	// Old committee from {a,b,c,d,e} ∩ gossip = {a,c,d}
	assert.Equal(t, []string{"a", "c", "d"}, accountsOf(old))
	// New committee from {a,b,c,d,f} ∩ gossip = {a,c,d,f}
	assert.Equal(t, []string{"a", "c", "d", "f"}, accountsOf(new_))
	// oldFullSize is pre-filter bitset count (5), not affected by readiness
	assert.Equal(t, 5, oldFullSize)
}
