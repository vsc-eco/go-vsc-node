package ledgerSystem_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	. "vsc-node/modules/ledger-system"
)

// review2 LOW #108 — Withdraw only rejected Amount <= 0; there was no
// minimum/dust threshold. A 0.001 dust withdrawal still triggered a full
// gateway multisig Hive transfer regardless of size — economically
// nonsensical and spammable. The fix rejects amounts below
// params.MINIMUM_WITHDRAWAL.
//
// Differential: a dust withdrawal (well-funded, valid destination) is
// accepted with Ok:true "success" on the #170 baseline (RED) and
// rejected with "amount below minimum withdrawal" on fix (GREEN). A
// normal-sized withdrawal still succeeds on both arms (sanity).
func TestReview2MinimumWithdrawal(t *testing.T) {
	const user = "hive:alice"

	newSession := func() interface {
		Withdraw(WithdrawParams) LedgerResult
	} {
		state := &LedgerState{
			Oplog:           make([]OpLogEvent, 0),
			VirtualLedger:   make(map[string][]LedgerUpdate),
			GatewayBalances: make(map[string]uint64),
			LedgerDb:        &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledger_db.LedgerRecord)},
			ActionDb:        &test_utils.MockActionsDb{Actions: make(map[string]ledger_db.ActionRecord)},
			BalanceDb: &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledger_db.BalanceRecord{
				user: {{Account: user, BlockHeight: 0, Hive: 100000}},
			}},
		}
		return NewSession(state)
	}

	dust := WithdrawParams{
		Id:    "w-dust",
		From:  user,
		To:    "alice",
		Asset: "hive",
		// Dust: > 0 (passes the baseline's only check) but under the
		// MINIMUM_WITHDRAWAL floor (10) the fix introduces.
		Amount: 5,
	}
	res := newSession().Withdraw(dust)
	if res.Ok {
		t.Fatalf("review2 #108: dust withdrawal (amount %d) accepted (Ok=%v msg=%q) — "+
			"baseline has no minimum and returns success", dust.Amount, res.Ok, res.Msg)
	}
	if res.Msg != "amount below minimum withdrawal" {
		t.Fatalf("review2 #108: dust withdrawal rejected with %q, want 'amount below minimum withdrawal'", res.Msg)
	}

	// Sanity: a normal withdrawal still succeeds (guard not over-broad).
	ok := WithdrawParams{
		Id:     "w-ok",
		From:   user,
		To:     "alice",
		Asset:  "hive",
		Amount: 1000,
	}
	if res := newSession().Withdraw(ok); !res.Ok {
		t.Fatalf("review2 #108: normal withdrawal rejected (Ok=%v msg=%q), want success", res.Ok, res.Msg)
	}
}
