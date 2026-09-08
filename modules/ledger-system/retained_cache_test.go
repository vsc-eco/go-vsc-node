package ledgerSystem_test

import (
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
)

// transferOp builds a transfer OpLogEvent.
func transferOp(id, from, to string, amount int64) ledgerSystem.OpLogEvent {
	return ledgerSystem.OpLogEvent{
		Id:          id,
		From:        from,
		To:          to,
		Amount:      amount,
		Asset:       "hive",
		Type:        "transfer",
		BlockHeight: 100,
	}
}

// TestRetainedBalanceCacheAcrossDone verifies the session balance cache is
// retained across Done() calls and self-maintains through VirtualLedger: a
// later transaction in the same session sees the previous tx's effects without
// re-reading the DB.
func TestRetainedBalanceCacheAcrossDone(t *testing.T) {
	state := newTestState()
	seedBalance(state, "hive:alice", 90, 1000, 0, 0)
	seedBalance(state, "hive:bob", 90, 500, 0, 0)
	sess := ledgerSystem.NewSession(state)

	// tx1: alice -> bob 100
	sess.ExecuteTransfer(transferOp("tx1-0", "hive:alice", "hive:bob", 100))
	sess.Done()

	// tx2 (same session): balances must include tx1's effects (cache retained
	// and folded through VirtualLedger).
	assert.Equal(t, int64(900), sess.GetBalance("hive:alice", 100, "hive"))
	assert.Equal(t, int64(600), sess.GetBalance("hive:bob", 100, "hive"))

	// tx3: another transfer in the same session must build on the updated cache.
	sess.ExecuteTransfer(transferOp("tx3-0", "hive:alice", "hive:bob", 50))
	sess.Done()
	assert.Equal(t, int64(850), sess.GetBalance("hive:alice", 100, "hive"))
	assert.Equal(t, int64(650), sess.GetBalance("hive:bob", 100, "hive"))
}

// TestRetainedCacheInvalidatedByDirectWrite verifies a mid-slot direct ledger
// write (StoreLedger bypassing the session, e.g. a deposit) invalidates the
// affected owner's cache entry via the write-version check.
func TestRetainedCacheInvalidatedByDirectWrite(t *testing.T) {
	state := newTestState()
	seedBalance(state, "hive:alice", 90, 1000, 0, 0)
	seedBalance(state, "hive:bob", 90, 500, 0, 0)
	sess := ledgerSystem.NewSession(state)

	// Fill bob's cache entry.
	assert.Equal(t, int64(500), sess.GetBalance("hive:bob", 100, "hive"))

	// A direct deposit to bob lands in the ledger DB, bypassing the session.
	assert.NoError(t, state.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id: "dep-1", Owner: "hive:bob", Amount: 50, Asset: "hive",
		Type: "deposit", BlockHeight: 100,
	}))

	// The retained cache must NOT serve the stale pre-deposit value.
	assert.Equal(t, int64(550), sess.GetBalance("hive:bob", 100, "hive"))
}

// TestRevertDoesNotLeakIntoRetainedCache verifies a reverted transaction's
// balance effects are dropped from the retained cache.
func TestRevertDoesNotLeakIntoRetainedCache(t *testing.T) {
	state := newTestState()
	seedBalance(state, "hive:alice", 90, 1000, 0, 0)
	seedBalance(state, "hive:bob", 90, 500, 0, 0)
	sess := ledgerSystem.NewSession(state)

	sess.ExecuteTransfer(transferOp("tx1-0", "hive:alice", "hive:bob", 100))
	sess.Done()

	// A failing tx (revert) moves alice->bob 200; must be fully undone.
	sess.ExecuteTransfer(transferOp("tx2-0", "hive:alice", "hive:bob", 200))
	sess.Revert()

	assert.Equal(t, int64(900), sess.GetBalance("hive:alice", 100, "hive"))
	assert.Equal(t, int64(600), sess.GetBalance("hive:bob", 100, "hive"))
}

// TestSavepointRestoreKeepsFillVersionsConsistent verifies a nested-call
// rollback restores the cache fill versions, so a direct write that happened
// inside the nested scope is still honored afterwards (no stale service).
func TestSavepointRestoreKeepsFillVersionsConsistent(t *testing.T) {
	state := newTestState()
	seedBalance(state, "hive:alice", 90, 1000, 0, 0)
	sess := ledgerSystem.NewSession(state)

	// Fill + commit tx1: alice -> -100 via transfer out.
	sess.ExecuteTransfer(transferOp("tx1-0", "hive:alice", "hive:bob", 100))
	sess.Done()

	sp := sess.Savepoint()

	// Inside the nested scope: a direct deposit to alice, then a touch of alice.
	assert.NoError(t, state.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id: "dep-1", Owner: "hive:alice", Amount: 25, Asset: "hive",
		Type: "deposit", BlockHeight: 100,
	}))
	sess.ExecuteTransfer(transferOp("nested-0", "hive:alice", "hive:bob", 10))

	// Nested scope fails; restore.
	sess.RestoreSavepoint(sp)

	// The direct deposit is REAL and must be visible; the nested transfer must
	// not leak. Without fill-version restore, the cache would serve 900.
	assert.Equal(t, int64(925), sess.GetBalance("hive:alice", 100, "hive"))
}
