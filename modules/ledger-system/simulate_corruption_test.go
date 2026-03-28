package ledgerSystem_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSimulateContractCallsCorruptsLiveState proves that the code path used by
// SimulateContractCalls (schema.resolvers.go:546-723) mutates the live
// LedgerState when session.Done() is called.
//
// Bug summary:
//   Line 569:  ledgerSession := ledgerSystem.NewSession(r.StateEngine.LedgerState)
//   Line 719:  ledgerSession.Done()
//
// NewSession stores a *pointer* to the live LedgerState.  Done() appends
// the session's oplog and ledger operations directly into that live state
// (ledger_session.go:30-34).  A read-only simulation endpoint should
// NEVER mutate production state.

func makeTestLedgerState() *ledgerSystem.LedgerState {
	return &ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		BlockHeight:     100,

		LedgerDb: &test_utils.MockLedgerDb{
			LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
		},
		ActionDb: &test_utils.MockActionsDb{
			Actions: make(map[string]ledgerDb.ActionRecord),
		},
		BalanceDb: &test_utils.MockBalanceDb{
			BalanceRecords: make(map[string][]ledgerDb.BalanceRecord),
		},
	}
}

func TestSimulateDoneCorruptsLiveOplog(t *testing.T) {
	state := makeTestLedgerState()

	// Seed: give alice 1000 HIVE via a balance record.
	balDb := state.BalanceDb.(*test_utils.MockBalanceDb)
	balDb.BalanceRecords["hive:alice"] = []ledgerDb.BalanceRecord{
		{Account: "hive:alice", BlockHeight: 50, Hive: 1000},
	}

	// Confirm starting conditions.
	require.Equal(t, 0, len(state.Oplog), "oplog should be empty before simulation")
	require.Equal(t, 0, len(state.VirtualLedger), "virtual ledger should be empty before simulation")

	startBalance := state.SnapshotForAccount("hive:alice", 100, "hive")
	require.Equal(t, int64(1000), startBalance, "alice should start with 1000 HIVE")

	// Replicate SimulateContractCalls code path.
	// schema.resolvers.go:569 — creates session from live state
	session := ledgerSystem.NewSession(state)

	// Simulate a contract transferring 500 HIVE from alice to bob.
	result := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id:          "sim-tx-1",
		From:        "hive:alice",
		To:          "hive:bob",
		Amount:      500,
		Asset:       "hive",
		BlockHeight: 100,
	})
	require.True(t, result.Ok, "transfer should succeed: %s", result.Msg)

	// Before Done(): live state should still be clean.
	assert.Equal(t, 0, len(state.Oplog),
		"BUG PRECONDITION: oplog should still be empty before Done()")
	assert.Equal(t, 0, len(state.VirtualLedger),
		"BUG PRECONDITION: virtual ledger should still be empty before Done()")

	// THIS IS THE BUG: calling Done() on a "simulation" session.
	// schema.resolvers.go:719
	session.Done()

	// Prove the corruption.
	assert.NotEqual(t, 0, len(state.Oplog),
		"BUG CONFIRMED: Done() appended to live Oplog — simulation corrupted production state")
	assert.Equal(t, 1, len(state.Oplog),
		"exactly one oplog entry should have leaked into live state")
	assert.Equal(t, "sim-tx-1", state.Oplog[0].Id,
		"the leaked oplog entry is from the simulation")

	// VirtualLedger should now contain entries for both alice and bob.
	assert.NotEqual(t, 0, len(state.VirtualLedger),
		"BUG CONFIRMED: Done() appended to live VirtualLedger")

	aliceUpdates := state.VirtualLedger["hive:alice"]
	bobUpdates := state.VirtualLedger["hive:bob"]
	assert.NotEmpty(t, aliceUpdates, "alice's virtual ledger was mutated by simulation")
	assert.NotEmpty(t, bobUpdates, "bob's virtual ledger was mutated by simulation")

	// The live balance is now wrong: alice lost 500 HIVE from a *simulation*.
	postBalance := state.SnapshotForAccount("hive:alice", 100, "hive")
	assert.NotEqual(t, startBalance, postBalance,
		"BUG CONFIRMED: alice's live balance changed from a read-only simulation")
	assert.Equal(t, int64(500), postBalance,
		"alice's balance dropped from 1000 to 500 due to simulation side-effect")

	// Bob gained 500 HIVE that should never have existed.
	bobBalance := state.SnapshotForAccount("hive:bob", 100, "hive")
	assert.Equal(t, int64(500), bobBalance,
		"BUG CONFIRMED: bob received 500 HIVE from a simulation that should have been side-effect-free")
}

func TestSimulateRevertDoesNotCorrupt(t *testing.T) {
	// Counter-test: Revert() does NOT corrupt the live state.
	// This shows the fix is to either always call Revert() after simulation,
	// or to clone the LedgerState before creating the session.
	state := makeTestLedgerState()

	balDb := state.BalanceDb.(*test_utils.MockBalanceDb)
	balDb.BalanceRecords["hive:alice"] = []ledgerDb.BalanceRecord{
		{Account: "hive:alice", BlockHeight: 50, Hive: 1000},
	}

	session := ledgerSystem.NewSession(state)

	result := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id:          "sim-tx-2",
		From:        "hive:alice",
		To:          "hive:bob",
		Amount:      500,
		Asset:       "hive",
		BlockHeight: 100,
	})
	require.True(t, result.Ok, "transfer should succeed: %s", result.Msg)

	// On error, the resolver calls Revert() — this is safe.
	session.Revert()

	assert.Equal(t, 0, len(state.Oplog),
		"Revert() correctly leaves live Oplog untouched")
	assert.Equal(t, 0, len(state.VirtualLedger),
		"Revert() correctly leaves live VirtualLedger untouched")

	postBalance := state.SnapshotForAccount("hive:alice", 100, "hive")
	assert.Equal(t, int64(1000), postBalance,
		"Revert() correctly preserves alice's live balance")
}

func TestMultipleSimulationsCompoundCorruption(t *testing.T) {
	// Proves that repeated simulate calls cause cumulative corruption.
	state := makeTestLedgerState()

	balDb := state.BalanceDb.(*test_utils.MockBalanceDb)
	balDb.BalanceRecords["hive:alice"] = []ledgerDb.BalanceRecord{
		{Account: "hive:alice", BlockHeight: 50, Hive: 1000},
	}

	// First simulation: transfer 200.
	s1 := ledgerSystem.NewSession(state)
	r1 := s1.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id: "sim-1", From: "hive:alice", To: "hive:bob",
		Amount: 200, Asset: "hive", BlockHeight: 100,
	})
	require.True(t, r1.Ok)
	s1.Done()

	// Second simulation: transfer another 200.
	s2 := ledgerSystem.NewSession(state)
	r2 := s2.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id: "sim-2", From: "hive:alice", To: "hive:bob",
		Amount: 200, Asset: "hive", BlockHeight: 100,
	})
	require.True(t, r2.Ok)
	s2.Done()

	assert.Equal(t, 2, len(state.Oplog),
		"BUG CONFIRMED: two simulations leaked two oplog entries into live state")

	bal := state.SnapshotForAccount("hive:alice", 100, "hive")
	assert.Equal(t, int64(600), bal,
		"BUG CONFIRMED: alice lost 400 HIVE total across two simulations (1000 -> 600)")

	bobBal := state.SnapshotForAccount("hive:bob", 100, "hive")
	assert.Equal(t, int64(400), bobBal,
		"BUG CONFIRMED: bob gained 400 phantom HIVE across two simulations")
}
