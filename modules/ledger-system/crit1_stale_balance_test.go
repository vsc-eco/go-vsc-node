package ledgerSystem_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	. "vsc-node/modules/ledger-system"
)

// newCrit1State builds a LedgerState whose persisted hbd ledger for `user`
// nets to 55000 but is composed of records the pre-fix delta filter
// mishandled: a deposit (counted by the old allow-list), two outflows
// transfer/withdraw (dropped by the old ["unstake","deposit"] allow-list), and
// an incoming transfer (also dropped). The snapshot base is 0, so GetBalance's
// delta is what's under test. Records are read straight from the DB (not
// through a session), so the in-memory VirtualLedger does not mask the result.
func newCrit1State(user string) *LedgerState {
	return &LedgerState{
		Oplog:           make([]OpLogEvent, 0),
		VirtualLedger:   make(map[string][]LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		ActionDb:        &test_utils.MockActionsDb{Actions: make(map[string]ledger_db.ActionRecord)},
		BalanceDb: &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledger_db.BalanceRecord{
			user: {{Account: user, BlockHeight: 0, HBD: 0}},
		}},
		LedgerDb: &test_utils.MockLedgerDb{LedgerRecords: map[string][]ledger_db.LedgerRecord{
			user: {
				{Id: "d1", To: user, Amount: 100000, Asset: "hbd", Type: "deposit", BlockHeight: 10},
				{Id: "t1#in", From: user, Amount: 40000, Asset: "hbd", Type: "transfer", BlockHeight: 20},
				{Id: "w1#in", From: user, Amount: 10000, Asset: "hbd", Type: "withdraw", BlockHeight: 30},
				{Id: "t2#out", To: user, Amount: 5000, Asset: "hbd", Type: "transfer", BlockHeight: 40},
			},
		}},
	}
}

// TestCrit1_DeltaNetsAllOutflows is the core CRIT-1 fix: GetBalance nets every
// ledger record of the asset past the snapshot, so outflows (transfer/withdraw)
// are subtracted and the incoming transfer credited — rather than the pre-fix
// behavior that summed deposits only and reported a stale, overstated balance
// (the double-spend primitive).
func TestCrit1_DeltaNetsAllOutflows(t *testing.T) {
	const user = "hive:alice"
	state := newCrit1State(user)

	got := state.GetBalance(user, 100, "hbd")
	const wantTrue = int64(55000) // 100000 - 40000 - 10000 + 5000
	if got != wantTrue {
		t.Fatalf("GetBalance = %d, want %d (true net; pre-fix returned 100000 stale)", got, wantTrue)
	}
}

// TestCrit1_MetaRowsExcluded confirms the delta still excludes the protocol
// meta rows (matching state_engine.UpdateBalances' fold), so a non-spendable
// burn row carrying tk=hive is not counted as spendable hive.
func TestCrit1_MetaRowsExcluded(t *testing.T) {
	const user = "hive:bob"
	state := &LedgerState{
		Oplog:           make([]OpLogEvent, 0),
		VirtualLedger:   make(map[string][]LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		ActionDb:        &test_utils.MockActionsDb{Actions: make(map[string]ledger_db.ActionRecord)},
		BalanceDb: &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledger_db.BalanceRecord{
			user: {{Account: user, BlockHeight: 0, Hive: 0}},
		}},
		LedgerDb: &test_utils.MockLedgerDb{LedgerRecords: map[string][]ledger_db.LedgerRecord{
			user: {
				{Id: "hd", To: user, Amount: 100000, Asset: "hive", Type: "deposit", BlockHeight: 10},
				// A protocol meta row carrying tk=hive that must NOT count.
				{Id: "burn", From: user, Amount: 100000, Asset: "hive", Type: LedgerTypeSafetySlashHiveBurn, BlockHeight: 20},
			},
		}},
	}

	got := state.GetBalance(user, 100, "hive")
	if got != 100000 {
		t.Fatalf("GetBalance(hive) = %d, want 100000 (burn meta row excluded)", got)
	}
}
