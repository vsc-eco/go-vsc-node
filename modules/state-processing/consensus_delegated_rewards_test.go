package state_engine_test

import (
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
)

// edgeRec is a delegation-edge ledger row (the virtual AssetDelegation asset the
// 0.3.0 stake path writes).
func edgeRec(id, from, to string, amount int64) ledgerDb.LedgerRecord {
	return ledgerDb.LedgerRecord{
		Id:          id,
		Owner:       ledgerSystem.DelegationEdgeKey(from, to),
		Amount:      amount,
		Asset:       ledgerSystem.AssetDelegation,
		Type:        "consensus_stake",
		BlockHeight: 1,
	}
}

// End-to-end read path for share-mode reward distribution: te.SE wires a REAL
// LedgerSystem over the mock ledger DB, so PendulumShareDelegations exercises
// AllDelegationEdges (ledger scan) + NodeDelegationMode (witness DB) exactly as
// the settlement producer and re-derivation do.
func TestPendulumShareDelegations_Wiring(t *testing.T) {
	const (
		alice = "hive:alice"
		carol = "hive:carol"
		opB   = "hive:operatorb" // share
		opC   = "hive:operatorc" // custom (delegation allowed, but rewards off-chain)
		opD   = "hive:operatord" // deactivated (no announcement)
	)

	te := newTestEnv()

	// Seed delegation edges: opB has two stakers (incl. its own self-edge); opC
	// also has a delegator; opD has none.
	require := te.LedgerDb.StoreLedger(
		edgeRec("e1", alice, opB, 600),
		edgeRec("e2", opB, opB, 400), // operator self-stake edge
		edgeRec("e3", carol, opC, 1000),
	)
	assert.NoError(t, require)

	// Announce modes.
	te.WitnessDb.ByAccount["operatorb"] = &witnesses.Witness{Account: "operatorb", DelegationMode: "share"}
	te.WitnessDb.ByAccount["operatorc"] = &witnesses.Witness{Account: "operatorc", DelegationMode: "custom"}
	// operatord intentionally has no witness record → default Deactivated.

	members := []string{opB, opC, opD}
	share, ok := te.SE.PendulumShareDelegations(members, 100)
	assert.True(t, ok, "share-delegation read must succeed")

	// Only the share-mode node is present, with its exact edges.
	assert.Len(t, share, 1, "only the share-mode node contributes")
	assert.Equal(t, map[string]int64{alice: 600, opB: 400}, share[opB],
		"share node returns its delegator + operator self-edge stakes")
	assert.NotContains(t, share, opC, "custom-mode node must be excluded (rewards stay with operator)")
	assert.NotContains(t, share, opD, "deactivated/unannounced node must be excluded")
}

// NodeDelegationMode reads the operator's announced mode from the witness DB and
// defaults to Deactivated (opt-in) when absent — the gate the stake handler uses.
func TestNodeDelegationMode_Wiring(t *testing.T) {
	te := newTestEnv()
	te.WitnessDb.ByAccount["operatorb"] = &witnesses.Witness{Account: "operatorb", DelegationMode: "share"}
	te.WitnessDb.ByAccount["operatorc"] = &witnesses.Witness{Account: "operatorc", DelegationMode: ""} // announced empty

	assert.Equal(t, "share", te.SE.NodeDelegationMode("hive:operatorb", 100))
	assert.Equal(t, "deactivated", te.SE.NodeDelegationMode("hive:operatorc", 100),
		"announced-empty normalizes to deactivated")
	assert.Equal(t, "deactivated", te.SE.NodeDelegationMode("hive:nobody", 100),
		"no announcement → deactivated (opt-in)")
}
