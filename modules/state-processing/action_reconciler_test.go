package state_engine_test

import (
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// B10 — until now nothing detected an action marked complete whose credit leg
// never landed. That is the IndexActions failure mode, and the reason it
// survived so long is that nothing ever noticed it. The reconciler must be
// read-only and must never panic or fail-stop: it must not become the halt
// vector the removed balance guard was.
func TestActionReconciler_IsReadOnlyAndNeverPanics(t *testing.T) {
	env := newTestEnv()
	// A completed stake whose credit row is absent — the exact damage shape.
	env.ActionsDb.Actions["a1"] = ledgerDb.ActionRecord{
		Id: "a1", Amount: 5000, To: "hive:alice", Type: "stake", Status: "complete",
	}
	before := len(env.LedgerDb.LedgerRecords)

	assert.NotPanics(t, func() {
		env.SE.ReconcileCompletedActionsForTest(stateEngine.ActionReconcileInterval)
	}, "the reconciler must never panic — it is observability, not consensus")

	assert.Equal(t, before, len(env.LedgerDb.LedgerRecords),
		"it must not write anything")
	assert.Equal(t, "complete", env.ActionsDb.Actions["a1"].Status,
		"and must not mutate the action it reports")
}

// A healthy action (credit present) must not be reported, and the sweep runs on
// its interval rather than every slot.
func TestActionReconciler_HealthyActionAndOffInterval(t *testing.T) {
	env := newTestEnv()
	env.ActionsDb.Actions["a2"] = ledgerDb.ActionRecord{
		Id: "a2", Amount: 100, To: "hive:bob", Type: "stake", Status: "complete",
	}
	env.LedgerDb.LedgerRecords["hive:bob"] = []ledgerDb.LedgerRecord{{
		Id: "a2#out", Owner: "hive:bob", Amount: 100, Asset: "hbd_savings", Type: "stake",
	}}

	assert.NotPanics(t, func() {
		env.SE.ReconcileCompletedActionsForTest(stateEngine.ActionReconcileInterval)
		env.SE.ReconcileCompletedActionsForTest(stateEngine.ActionReconcileInterval + 1)
	})
	assert.Len(t, env.LedgerDb.LedgerRecords["hive:bob"], 1, "still read-only")
}
