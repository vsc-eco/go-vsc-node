package state_engine_test

import (
	"strings"
	"testing"
	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"github.com/stretchr/testify/assert"
)

// ★★★ THE HELPERS BEING RIGHT DOES NOT MEAN THE GUARD IS WIRED IN.
//
// balance_guard_test.go covers negativeSpendableBalance and isGuardedAccount as
// pure functions. Both can be perfect while UpdateBalances never calls them —
// the exact failure mode where a unit test goes green and the caller was never
// hooked up. These tests drive the real UpdateBalances and assert on its
// observable behaviour, so the guard cannot be silently disconnected.
//
// Written after the 2026-08-13 mainnet halt: five nodes carried a daveks
// hbd_savings 53 units below the other twelve, and when the finalized unstake of
// his exact balance applied verbatim (ledger-system/utils.go, `Amount: -v.Amount`,
// no floor) they would have materialized -53. The guard exists to stop that node
// dead rather than let it persist corruption and poison every subsequent
// interest distribution via totalAvgBig.

// A corrupted node materializing a negative spendable balance must PANIC out of
// UpdateBalances (caught upstream by the streamer supervisor's inteceptError),
// and must NOT persist the negative snapshot.
func TestUpdateBalances_PanicsOnNegativeSpendable_AndDoesNotPersist(t *testing.T) {
	const (
		acct  = "hive:daveks"
		start = uint64(100)
		end   = uint64(110)
	)

	env := newTestEnv()

	// Seed the snapshot 53 units short — the exact mainnet shape.
	env.BalanceDb.BalanceRecords[acct] = []ledgerDb.BalanceRecord{{
		Account:           acct,
		BlockHeight:       start - 1,
		HBD_SAVINGS:       5271,
		HBD_MODIFY_HEIGHT: start - 1,
		HBD_CLAIM_HEIGHT:  start - 1,
	}}
	before := len(env.BalanceDb.BalanceRecords[acct])

	// The finalized unstake of the full 5324 debits more than this node holds.
	env.LedgerDb.LedgerRecords[acct] = []ledgerDb.LedgerRecord{{
		Id:          "unstake#in",
		Owner:       acct,
		Amount:      -5324,
		Asset:       "hbd_savings",
		BlockHeight: start + 1,
		Type:        "unstake",
	}}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("UpdateBalances did NOT panic on a negative spendable balance — " +
				"the guard is not wired into the write path")
		}
		err, ok := r.(error)
		if !ok {
			t.Fatalf("panicked with %T, want error: %v", r, r)
		}
		msg := err.Error()
		assert.Contains(t, msg, "MAGI-HALT", "panic must be identifiable by operators")
		assert.Contains(t, msg, acct, "panic must name the offending account")
		assert.Contains(t, msg, "hbd_savings", "panic must name the offending asset")
		assert.True(t, strings.Contains(msg, "-53"),
			"panic must report the materialized value; got: %s", msg)

		// The whole point of halting BEFORE the write: no corrupt snapshot lands.
		assert.Equal(t, before, len(env.BalanceDb.BalanceRecords[acct]),
			"guard must halt before persisting the negative snapshot")
	}()

	env.SE.UpdateBalances(start, end)
}

// The mirror image, and the one that matters for rollout safety: a healthy node
// must be completely unaffected. daveks' real unstake takes him to exactly 0 —
// a whole-balance "max" op, which is the normal path, not an edge case. If the
// guard fired at zero it would halt every honest node the first time anyone
// withdrew everything.
func TestUpdateBalances_ExactZeroIsHealthy_NoPanic(t *testing.T) {
	const (
		acct  = "hive:daveks"
		start = uint64(100)
		end   = uint64(110)
	)

	env := newTestEnv()
	env.BalanceDb.BalanceRecords[acct] = []ledgerDb.BalanceRecord{{
		Account:           acct,
		BlockHeight:       start - 1,
		HBD_SAVINGS:       5324, // the majority's value
		HBD_MODIFY_HEIGHT: start - 1,
		HBD_CLAIM_HEIGHT:  start - 1,
	}}
	env.LedgerDb.LedgerRecords[acct] = []ledgerDb.LedgerRecord{{
		Id:          "unstake#in",
		Owner:       acct,
		Amount:      -5324,
		Asset:       "hbd_savings",
		BlockHeight: start + 1,
		Type:        "unstake",
	}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("guard fired on a healthy exact-zero balance — this would halt "+
				"every honest node on any whole-balance withdrawal: %v", r)
		}
	}()

	env.SE.UpdateBalances(start, end)
}

// Non-hive accounts are deliberately out of scope (isGuardedAccount). Contract
// and system accounts run their own accounting and a negative there must not
// take the node down.
func TestUpdateBalances_NonHiveAccountNotGuarded(t *testing.T) {
	const (
		acct  = "contract:vsc1Brvi4YZHLkocYNAFd7Gf1JpsPjzNnv4i45"
		start = uint64(100)
		end   = uint64(110)
	)

	env := newTestEnv()
	env.BalanceDb.BalanceRecords[acct] = []ledgerDb.BalanceRecord{{
		Account:     acct,
		BlockHeight: start - 1,
		HBD_SAVINGS: 10,
	}}
	env.LedgerDb.LedgerRecords[acct] = []ledgerDb.LedgerRecord{{
		Id:          "drain#in",
		Owner:       acct,
		Amount:      -99,
		Asset:       "hbd_savings",
		BlockHeight: start + 1,
		Type:        "unstake",
	}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("guard fired on a non-hive account; it is scoped to hive: only: %v", r)
		}
	}()

	env.SE.UpdateBalances(start, end)
}
