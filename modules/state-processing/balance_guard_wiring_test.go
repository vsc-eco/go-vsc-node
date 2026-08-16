package state_engine_test

import (
	"testing"
	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"github.com/stretchr/testify/assert"
)

// ★★★ A NEGATIVE SPENDABLE BALANCE MUST NEVER HALT THE BLOCK PIPELINE.
//
// balance_guard_test.go covers negativeSpendableBalance and isGuardedAccount as
// pure functions. These tests drive the real UpdateBalances and pin its
// observable behaviour end-to-end.
//
// This file originally asserted the opposite: that UpdateBalances PANICS on a
// negative, added during the 2026-08-13 recovery when we believed a negative
// could only mean local divergence. That premise was wrong, and the panic
// halted mainnet on 2026-08-16 — hive:dhedge had carried a network-consistent
// hbd_savings of -283 for months (it staked 63364 and unstaked 64302, an
// over-debit of 938), and an unrelated hbd transfer re-materialized the record
// on every node at once. The guard is now log-only; these tests exist to keep
// it that way.

// A negative spendable balance must be PERSISTED and processing must CONTINUE.
// Halting here takes the whole fleet down, because such balances are
// network-consistent legacy state, not local corruption.
func TestUpdateBalances_NegativeSpendable_DoesNotPanic_AndPersists(t *testing.T) {
	const (
		acct  = "hive:daveks"
		start = uint64(100)
		end   = uint64(110)
	)

	env := newTestEnv()

	// Seed the snapshot 53 units short — the exact 2026-08-13 mainnet shape.
	env.BalanceDb.BalanceRecords[acct] = []ledgerDb.BalanceRecord{{
		Account:           acct,
		BlockHeight:       start - 1,
		HBD_SAVINGS:       5271,
		HBD_MODIFY_HEIGHT: start - 1,
		HBD_CLAIM_HEIGHT:  start - 1,
	}}

	// A finalized unstake of the full 5324 debits more than this node holds.
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
			t.Fatalf("UpdateBalances panicked on a negative spendable balance — "+
				"the halt must stay removed (it halted mainnet on 2026-08-16): %v", r)
		}
	}()

	env.SE.UpdateBalances(start, end)

	// The negative must land in the snapshot: processing continues, and the
	// accounting defect is settled later by a compensating ledger record, not
	// by refusing to write.
	recs := env.BalanceDb.BalanceRecords[acct]
	if len(recs) == 0 {
		t.Fatal("no balance record persisted for the account")
	}
	latest := recs[len(recs)-1]
	assert.Equal(t, int64(-53), latest.HBD_SAVINGS,
		"the materialized negative must be persisted, not dropped")
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
