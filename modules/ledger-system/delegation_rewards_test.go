package ledgerSystem

import (
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"github.com/stretchr/testify/assert"
)

func TestGroupDelegationEdges(t *testing.T) {
	const userA = "hive:usera"
	const userC = "hive:userc"
	const opB = "hive:operatorb"
	const opD = "hive:operatord"

	records := []ledgerDb.LedgerRecord{
		// userA -> opB: stake 10.000 then unstake 4.000 → net 6.000
		{Id: "s1#edge", Owner: DelegationEdgeKey(userA, opB), Amount: 10000, Asset: AssetDelegation, Type: "consensus_stake"},
		{Id: "u1#edge", Owner: DelegationEdgeKey(userA, opB), Amount: -4000, Asset: AssetDelegation, Type: "consensus_unstake"},
		// userC -> opB: 5.000
		{Id: "s2#edge", Owner: DelegationEdgeKey(userC, opB), Amount: 5000, Asset: AssetDelegation, Type: "consensus_stake"},
		// opB self-edge: 3.000
		{Id: "s3#edge", Owner: DelegationEdgeKey(opB, opB), Amount: 3000, Asset: AssetDelegation, Type: "consensus_stake"},
		// userA -> opD: staked 2.000 then fully unstaked → net 0, dropped
		{Id: "s4#edge", Owner: DelegationEdgeKey(userA, opD), Amount: 2000, Asset: AssetDelegation, Type: "consensus_stake"},
		{Id: "u4#edge", Owner: DelegationEdgeKey(userA, opD), Amount: -2000, Asset: AssetDelegation, Type: "consensus_unstake"},

		// Noise that must be ignored: the plain hive/hive_consensus legs and the
		// gross per-node total rows of the same ops.
		{Id: "s1#in", Owner: userA, Amount: -10000, Asset: "hive", Type: "consensus_stake"},
		{Id: "s1#out", Owner: opB, Amount: 10000, Asset: "hive_consensus", Type: "consensus_stake"},
		{Id: "s1#total", Owner: opB, Amount: 10000, Asset: AssetDelegationTotal, Type: "consensus_stake"},
	}

	edges := groupDelegationEdges(records)

	// opB has three positive edges; opD has none (fully unstaked).
	assert.Len(t, edges, 1, "only opB should have positive edges")
	assert.NotContains(t, edges, opD, "fully-unstaked node must be absent")

	opBEdges := edges[opB]
	assert.Equal(t, int64(6000), opBEdges[userA], "userA net after partial unstake")
	assert.Equal(t, int64(5000), opBEdges[userC], "userC full stake")
	assert.Equal(t, int64(3000), opBEdges[opB], "operator self-edge")
	assert.Len(t, opBEdges, 3, "exactly three delegators (incl. operator self)")

	// Sum of edges = the share-split denominator.
	var total int64
	for _, s := range opBEdges {
		total += s
	}
	assert.Equal(t, int64(14000), total, "denominator = 6000+5000+3000")
}

func TestGroupDelegationEdges_Empty(t *testing.T) {
	assert.Empty(t, groupDelegationEdges(nil))
	assert.Empty(t, groupDelegationEdges([]ledgerDb.LedgerRecord{
		{Id: "x#in", Owner: "hive:a", Amount: -1, Asset: "hive", Type: "consensus_stake"},
	}), "no AssetDelegation rows → no edges")
}
