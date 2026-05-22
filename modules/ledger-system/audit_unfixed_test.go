package ledgerSystem_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// TestAuditUnfixed_34_DustWithdrawalAcceptedNoMinFee proves audit item #34:
// ledger-system/ledger_session.go:101-108 — Withdraw rejects only Amount<=0;
// it has no MinWithdrawal floor. An attacker can submit Amount=1 (the smallest
// asset unit, i.e. 0.001 HBD) withdrawals indefinitely. Each one schedules a
// real Hive multisig outbound transfer, so the on-chain operational cost
// (witness signatures + Hive RC + tx-fee surface) is paid by the protocol
// while the attacker burns at most one dust unit per request.
//
// Post-fix expectation: a min-amount floor (MinWithdrawal, sized so that
// per-action operational cost remains a small fraction of value moved) is
// enforced inside Withdraw, returning Ok=false with a clear error.
func TestAuditUnfixed_34_DustWithdrawalAcceptedNoMinFee(t *testing.T) {
	const user = "hive:alice"
	const bh = uint64(100)

	state := &ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		BlockHeight:     bh,

		LedgerDb: &test_utils.MockLedgerDb{
			LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
		},
		ActionDb: &test_utils.MockActionsDb{
			Actions: make(map[string]ledgerDb.ActionRecord),
		},
		BalanceDb: &test_utils.MockBalanceDb{
			BalanceRecords: map[string][]ledgerDb.BalanceRecord{
				// Plenty of HBD on hand so balance is not the gating concern.
				user: {{Account: user, BlockHeight: 0, HBD: 1_000_000}},
			},
		},
	}

	session := ledgerSystem.NewSession(state)

	// Amount=1 = 0.001 HBD = smallest expressible unit. Should succeed
	// today — that's the bug.
	res := session.Withdraw(ledgerSystem.WithdrawParams{
		Id:          "dust-wd-1",
		From:        user,
		To:          "alice", // bare hive name; Withdraw will normalise to hive:alice
		Asset:       "hbd",
		Amount:      1,
		BlockHeight: bh,
	})

	if !res.Ok {
		// If the assertion suddenly fails, the fix may already be in: a
		// MinWithdrawal check would surface as Ok=false here. Update this
		// test to assert the new floor behaviour.
		t.Fatalf("UNEXPECTED: dust withdraw (Amount=1) rejected with %q — "+
			"a min-withdrawal floor may have been introduced; "+
			"update this test to assert the new behaviour", res.Msg)
	}

	t.Log("#34 confirmed: Withdraw(Amount=1) returns Ok=true. There is no " +
		"per-action minimum amount, so an attacker can schedule unlimited " +
		"dust multisig outbounds. Fix: introduce MinWithdrawal (per-asset) " +
		"and reject sub-floor amounts before the oplog append at " +
		"ledger_session.go:178.")
}
