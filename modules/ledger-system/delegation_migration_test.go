package ledgerSystem

import (
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"github.com/stretchr/testify/assert"
)

func TestBackfillDelegationEdges(t *testing.T) {
	const userA = "hive:usera"
	const userC = "hive:userc"
	const opB = "hive:operatorb"

	// Historical records (pre-0.2.0): each stake is an #in/#out pair sharing a
	// base id; a legacy unstake is a lone #in on the node's own bond.
	records := []ledgerDb.LedgerRecord{
		// userA delegated 10.000 to operatorB
		{Id: "tx1#in", Owner: userA, Amount: -10000, Asset: "hive", Type: "consensus_stake"},
		{Id: "tx1#out", Owner: opB, Amount: 10000, Asset: "hive_consensus", Type: "consensus_stake"},
		// userC delegated 5.000 to operatorB
		{Id: "tx2#in", Owner: userC, Amount: -5000, Asset: "hive", Type: "consensus_stake"},
		{Id: "tx2#out", Owner: opB, Amount: 5000, Asset: "hive_consensus", Type: "consensus_stake"},
		// operatorB self-staked 3.000 ...
		{Id: "tx3#in", Owner: opB, Amount: -3000, Asset: "hive", Type: "consensus_stake"},
		{Id: "tx3#out", Owner: opB, Amount: 3000, Asset: "hive_consensus", Type: "consensus_stake"},
		// ... then legacy-unstaked 1.000 of its own
		{Id: "tx4#in", Owner: opB, Amount: -1000, Asset: "hive_consensus", Type: "consensus_unstake"},
	}

	out := BackfillDelegationEdges(records, 999)

	edge := map[string]int64{}
	total := map[string]int64{}
	for _, r := range out {
		assert.Equal(t, uint64(999), r.BlockHeight)
		switch r.Asset {
		case AssetDelegation:
			edge[r.Owner] = r.Amount
		case AssetDelegationTotal:
			total[r.Owner] = r.Amount
		default:
			t.Fatalf("unexpected asset %q in backfill output", r.Asset)
		}
	}

	assert.Equal(t, int64(10000), edge[DelegationEdgeKey(userA, opB)], "userA->operatorB delegation reclaimable")
	assert.Equal(t, int64(5000), edge[DelegationEdgeKey(userC, opB)], "userC->operatorB delegation reclaimable")
	assert.Equal(t, int64(2000), edge[DelegationEdgeKey(opB, opB)], "operatorB self-edge net of its legacy unstake")
	assert.Len(t, edge, 3, "exactly three positive edges")

	// Gross per-node total (slash-immune) = sum of all edges to operatorB.
	assert.Equal(t, int64(17000), total[opB], "operatorB gross delegated total = 10000+5000+2000")
	assert.Len(t, total, 1, "one node total")

	// Idempotency: re-running over output (AssetDelegation rows) must seed nothing.
	asDelegationRecords := make([]ledgerDb.LedgerRecord, 0, len(out))
	for _, u := range out {
		asDelegationRecords = append(asDelegationRecords, ledgerDb.LedgerRecord{
			Id: u.Id, Owner: u.Owner, Amount: u.Amount, Asset: u.Asset, Type: u.Type,
		})
	}
	assert.Empty(t, BackfillDelegationEdges(asDelegationRecords, 1000),
		"already-migrated delegation rows must never be re-counted")
}

func TestSplitLedgerBaseId(t *testing.T) {
	b, s := splitLedgerBaseId("tx1#in")
	assert.Equal(t, "tx1", b)
	assert.Equal(t, "in", s)

	// idCache ":N" suffix on the base is preserved.
	b, s = splitLedgerBaseId("tx1:2#out")
	assert.Equal(t, "tx1:2", b)
	assert.Equal(t, "out", s)

	b, s = splitLedgerBaseId("noseparator")
	assert.Equal(t, "noseparator", b)
	assert.Equal(t, "", s)
}
