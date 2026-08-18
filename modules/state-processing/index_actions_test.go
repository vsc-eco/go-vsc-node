package state_engine_test

import (
	"errors"
	"testing"

	test_utils "vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
)

// IndexActions is the SOLE writer of the stake/unstake credit leg, and until
// this file nothing exercised it: the only production call site is matched by
// three mock implementations with empty bodies, so every "balance conservation"
// test in the repo passed with the credit leg doing nothing at all.
//
// The three defects these tests pin, all of which drop a user's credit on one
// node and nowhere else — the exact divergence shape that halted mainnet twice
// in 2026-08:
//
//  1. the Get error was discarded, so a DB fault was indistinguishable from
//     "action not found" and the credit was silently skipped;
//  2. ExecuteComplete fired BEFORE the credit was written, so a failure
//     stranded the value with the action already marked done;
//  3. a failed credit write only logged.

func newIndexActionsEnv() (ledgerSystem.LedgerSystem, *test_utils.MockLedgerDb, *test_utils.MockActionsDb) {
	balDb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{}}
	lDb := &test_utils.MockLedgerDb{LedgerRecords: map[string][]ledgerDb.LedgerRecord{}}
	aDb := &test_utils.MockActionsDb{
		Actions:     map[string]ledgerDb.ActionRecord{},
		CompletedAt: map[string]int{},
	}
	aDb.LedgerRef = lDb
	return ledgerSystem.New(balDb, lDb, nil, aDb, nil), lDb, aDb
}

func creditRow(lDb *test_utils.MockLedgerDb, owner, id string) *ledgerDb.LedgerRecord {
	for _, r := range lDb.LedgerRecords[owner] {
		if r.Id == id {
			return &r
		}
	}
	return nil
}

// A stake action must produce its hbd_savings credit, and must only be marked
// complete once that credit exists.
func TestIndexActions_Stake_WritesCreditThenCompletes(t *testing.T) {
	ls, lDb, aDb := newIndexActionsEnv()
	aDb.Actions["act1"] = ledgerDb.ActionRecord{
		Id: "act1", Amount: 5000, To: "hive:alice", Type: "stake", Status: "pending",
	}

	ls.IndexActions(
		ledgerSystem.ActionUpdate{Ops: []string{"act1"}, ClearedOps: ""},
		ledgerSystem.ExtraInfo{BlockHeight: 100, ActionId: "tx1"},
	)

	row := creditRow(lDb, "hive:alice", "act1#out")
	if assert.NotNil(t, row, "the stake credit leg must be written") {
		assert.Equal(t, int64(5000), row.Amount)
		assert.Equal(t, "hbd_savings", row.Asset)
		assert.Equal(t, uint64(101), row.BlockHeight, "credit lands on the next block")
	}
	assert.Equal(t, "complete", aDb.Actions["act1"].Status)
	assert.Equal(t, 1, aDb.CompletedAt["act1"],
		"the credit must already exist when the action is completed — completing "+
			"first strands the value if the write then fails")
}

// Same for unstake, which credits liquid hbd after the unstake delay.
func TestIndexActions_Unstake_WritesCreditThenCompletes(t *testing.T) {
	ls, lDb, aDb := newIndexActionsEnv()
	aDb.Actions["act2"] = ledgerDb.ActionRecord{
		Id: "act2", Amount: 7000, To: "hive:bob", Type: "unstake", Status: "pending",
	}

	ls.IndexActions(
		ledgerSystem.ActionUpdate{Ops: []string{"act2"}, ClearedOps: ""},
		ledgerSystem.ExtraInfo{BlockHeight: 200, ActionId: "tx2"},
	)

	row := creditRow(lDb, "hive:bob", "act2#out")
	if assert.NotNil(t, row, "the unstake credit leg must be written") {
		assert.Equal(t, int64(7000), row.Amount)
		assert.Equal(t, "hbd", row.Asset)
	}
	assert.Equal(t, "complete", aDb.Actions["act2"].Status)
	assert.Equal(t, 1, aDb.CompletedAt["act2"], "credit before completion")
}

// ★ THE HALT MECHANISM. A transient DB fault on Get must NOT be treated as
// "action not found". The old code discarded the error and `continue`d, which
// silently skipped the credit forever — no retry, no log, nothing that ever
// notices, until a zero-margin op on that account forks the chain.
func TestIndexActions_TransientGetError_DoesNotSkipTheCredit(t *testing.T) {
	ls, lDb, aDb := newIndexActionsEnv()
	aDb.Actions["act3"] = ledgerDb.ActionRecord{
		Id: "act3", Amount: 1234, To: "hive:carol", Type: "stake", Status: "pending",
	}
	// First read faults, the retry succeeds.
	aDb.GetErrs = []error{errors.New("connection reset by peer")}

	ls.IndexActions(
		ledgerSystem.ActionUpdate{Ops: []string{"act3"}, ClearedOps: ""},
		ledgerSystem.ExtraInfo{BlockHeight: 300, ActionId: "tx3"},
	)

	row := creditRow(lDb, "hive:carol", "act3#out")
	assert.NotNil(t, row,
		"a transient Get failure must be retried, never mistaken for 'not found' — "+
			"skipping here is how the credit went missing")
	if row != nil {
		assert.Equal(t, int64(1234), row.Amount)
	}
	assert.Equal(t, "complete", aDb.Actions["act3"].Status)
}

// A genuinely absent action is not an error: nothing to credit, nothing to
// complete, and it must not be confused with the fault case above.
func TestIndexActions_MissingAction_IsSkippedCleanly(t *testing.T) {
	ls, lDb, aDb := newIndexActionsEnv()

	ls.IndexActions(
		ledgerSystem.ActionUpdate{Ops: []string{"nope"}, ClearedOps: ""},
		ledgerSystem.ExtraInfo{BlockHeight: 400, ActionId: "tx4"},
	)

	total := 0
	for _, recs := range lDb.LedgerRecords {
		total += len(recs)
	}
	assert.Zero(t, total, "an absent action must write nothing")
	assert.Empty(t, aDb.CompletedAt, "and complete nothing")
}

// Action types with no credit leg here (withdraw, consensus_unstake are settled
// elsewhere) must still be completed — this used to fall out of the type
// branches implicitly, so a new type without a credit would silently never
// complete.
func TestIndexActions_OtherTypes_StillComplete(t *testing.T) {
	ls, lDb, aDb := newIndexActionsEnv()
	aDb.Actions["act5"] = ledgerDb.ActionRecord{
		Id: "act5", Amount: 999, To: "hive:dave", Type: "withdraw", Status: "pending",
	}

	ls.IndexActions(
		ledgerSystem.ActionUpdate{Ops: []string{"act5"}, ClearedOps: ""},
		ledgerSystem.ExtraInfo{BlockHeight: 500, ActionId: "tx5"},
	)

	assert.Equal(t, "complete", aDb.Actions["act5"].Status,
		"a type with no credit leg here must still be marked complete")
	assert.Nil(t, creditRow(lDb, "hive:dave", "act5#out"),
		"but no credit leg may be invented for it")
}
