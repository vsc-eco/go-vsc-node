package settlement

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// sumDist totals a distribution slice.
func sumDist(d []Distribution) int64 {
	var s int64
	for _, x := range d {
		s += x.Amount
	}
	return s
}

func distMap(d []Distribution) map[string]int64 {
	m := make(map[string]int64, len(d))
	for _, x := range d {
		m[x.Account] = x.Amount
	}
	return m
}

func TestExpandShareDistributions_ExactProRata(t *testing.T) {
	// n1 earns 1000; operator self-stake 600, alice 400 → split exactly.
	dists := []Distribution{{Account: "hive:n1", Amount: 1000}}
	share := map[string]map[string]int64{
		"hive:n1": {"hive:n1": 600, "hive:alice": 400},
	}
	out := ExpandShareDistributions(dists, share)
	m := distMap(out)
	assert.Equal(t, int64(600), m["hive:n1"], "operator earns via own self-edge")
	assert.Equal(t, int64(400), m["hive:alice"], "delegator pro-rata by stake")
	assert.Equal(t, int64(1000), sumDist(out), "node total preserved exactly")
}

func TestExpandShareDistributions_RemainderToOperator(t *testing.T) {
	// 1000 across three equal edges: floor(1000/3)=333 each, remainder 1 → operator.
	dists := []Distribution{{Account: "hive:n1", Amount: 1000}}
	share := map[string]map[string]int64{
		"hive:n1": {"hive:n1": 1, "hive:alice": 1, "hive:bob": 1},
	}
	out := ExpandShareDistributions(dists, share)
	m := distMap(out)
	assert.Equal(t, int64(334), m["hive:n1"], "operator gets self-edge share 333 + rounding remainder 1")
	assert.Equal(t, int64(333), m["hive:alice"])
	assert.Equal(t, int64(333), m["hive:bob"])
	assert.Equal(t, int64(1000), sumDist(out), "conservation: remainder is not lost")
}

func TestExpandShareDistributions_PassthroughNonShare(t *testing.T) {
	// Node not in the share map keeps its whole amount (Custom / Deactivated).
	dists := []Distribution{{Account: "hive:n2", Amount: 500}}
	out := ExpandShareDistributions(dists, map[string]map[string]int64{})
	assert.Equal(t, []Distribution{{Account: "hive:n2", Amount: 500}}, out)
}

func TestExpandShareDistributions_Mixed(t *testing.T) {
	// One share node, one custom node in the same settlement.
	dists := []Distribution{
		{Account: "hive:n1", Amount: 1000}, // share
		{Account: "hive:n2", Amount: 500},  // custom (absent from share map)
	}
	share := map[string]map[string]int64{
		"hive:n1": {"hive:n1": 50, "hive:alice": 50},
	}
	out := ExpandShareDistributions(dists, share)
	m := distMap(out)
	assert.Equal(t, int64(500), m["hive:n1"], "operator self-edge half of 1000")
	assert.Equal(t, int64(500), m["hive:alice"])
	assert.Equal(t, int64(500), m["hive:n2"], "custom node unchanged")
	assert.Equal(t, int64(1500), sumDist(out), "global total preserved")
}

func TestExpandShareDistributions_MergeAcrossNodes(t *testing.T) {
	// alice delegates to TWO share nodes → exactly one merged entry (no
	// duplicate account, which would break the byte-stable sort + collide ids).
	dists := []Distribution{
		{Account: "hive:n1", Amount: 1000},
		{Account: "hive:n3", Amount: 1000},
	}
	share := map[string]map[string]int64{
		"hive:n1": {"hive:n1": 500, "hive:alice": 500},
		"hive:n3": {"hive:n3": 500, "hive:alice": 500},
	}
	out := ExpandShareDistributions(dists, share)
	// Verify uniqueness of accounts.
	seen := map[string]bool{}
	for _, d := range out {
		assert.False(t, seen[d.Account], "account %s appears more than once", d.Account)
		seen[d.Account] = true
	}
	m := distMap(out)
	assert.Equal(t, int64(1000), m["hive:alice"], "alice = 500 from n1 + 500 from n3, merged")
	assert.Equal(t, int64(500), m["hive:n1"])
	assert.Equal(t, int64(500), m["hive:n3"])
	assert.Equal(t, int64(2000), sumDist(out))
}

func TestExpandShareDistributions_ConservationProperty(t *testing.T) {
	// For arbitrary inputs the expanded total must equal the node-level total.
	dists := []Distribution{
		{Account: "hive:n1", Amount: 9973},
		{Account: "hive:n2", Amount: 4441},
		{Account: "hive:n3", Amount: 12},
	}
	share := map[string]map[string]int64{
		"hive:n1": {"hive:n1": 7, "hive:alice": 11, "hive:bob": 13},
		"hive:n2": {"hive:carol": 1}, // single delegator, no operator self-edge
	}
	out := ExpandShareDistributions(dists, share)
	assert.Equal(t, int64(9973+4441+12), sumDist(out), "no base unit created or destroyed")
	// Every emitted amount is strictly positive.
	for _, d := range out {
		assert.Greater(t, d.Amount, int64(0))
	}
}

func TestExpandShareDistributions_OperatorRemainderWithoutSelfEdge(t *testing.T) {
	// Node with delegators but no operator self-stake: rounding crumb still
	// lands on the node account (the operator), preserving conservation.
	dists := []Distribution{{Account: "hive:n2", Amount: 11}}
	share := map[string]map[string]int64{
		"hive:n2": {"hive:alice": 1, "hive:bob": 1},
	}
	out := ExpandShareDistributions(dists, share)
	m := distMap(out)
	assert.Equal(t, int64(5), m["hive:alice"])
	assert.Equal(t, int64(5), m["hive:bob"])
	assert.Equal(t, int64(1), m["hive:n2"], "remainder crumb to operator account")
	assert.Equal(t, int64(11), sumDist(out))
}

// TestComposeRecord_ShareDelegationsPreservesTotals proves the settlement
// record's conservation totals are byte-stable whether or not a node shares:
// only the recipient breakdown changes, never TotalDistributed / Residual.
func TestComposeRecord_ShareDelegationsPreservesTotals(t *testing.T) {
	base := ComposeInputs{
		Epoch:        7,
		PrevEpoch:    6,
		EpochStartBh: 1000,
		SlotHeight:   2000,
		CommitteeBonds: map[string]int64{
			"hive:n1": 100,
			"hive:n2": 100,
		},
		BucketBalanceHBD: 1000,
	}

	// Without share delegations: node-level distributions.
	plain, err := ComposeRecord(base)
	assert.NoError(t, err)
	assert.Equal(t, int64(1000), plain.TotalDistributedHBD)
	assert.Equal(t, int64(0), plain.ResidualHBD)
	plainMap := map[string]int64{}
	for _, d := range plain.Distributions {
		plainMap[d.Account] = d.HBDAmt
	}
	assert.Equal(t, int64(500), plainMap["hive:n1"])
	assert.Equal(t, int64(500), plainMap["hive:n2"])

	// With n1 in share mode (operator 50 / alice 50): n1's 500 → 250 + 250.
	withShare := base
	withShare.ShareDelegations = map[string]map[string]int64{
		"hive:n1": {"hive:n1": 50, "hive:alice": 50},
	}
	shared, err := ComposeRecord(withShare)
	assert.NoError(t, err)

	// Totals and residual are IDENTICAL — conservation invariant untouched.
	assert.Equal(t, plain.TotalDistributedHBD, shared.TotalDistributedHBD)
	assert.Equal(t, plain.ResidualHBD, shared.ResidualHBD)
	assert.Equal(t, plain.BucketBalanceHBD, shared.BucketBalanceHBD)

	sharedMap := map[string]int64{}
	for _, d := range shared.Distributions {
		sharedMap[d.Account] = d.HBDAmt
	}
	assert.Equal(t, int64(500), sharedMap["hive:n2"], "non-share node unchanged")
	assert.Equal(t, int64(250), sharedMap["hive:n1"], "share-node operator self-edge")
	assert.Equal(t, int64(250), sharedMap["hive:alice"], "delegator share")

	// Distributions remain sorted by account (byte-stable encoding contract).
	for i := 1; i < len(shared.Distributions); i++ {
		assert.Less(t, shared.Distributions[i-1].Account, shared.Distributions[i].Account)
	}
}
