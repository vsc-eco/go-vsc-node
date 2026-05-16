package gateway

import (
	"testing"

	systemconfig "vsc-node/modules/common/system-config"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/lib/test_utils"

	"github.com/vsc-eco/hivego"
)

// stubHiveCreator is an inert HiveTransactionCreator: it builds no real Hive
// ops and never broadcasts. executeActions only needs it to assemble a
// signingPackage; the double-spend behaviour under test is entirely about
// which ledger actions get *selected*, not about the produced Hive tx.
type stubHiveCreator struct{}

func (stubHiveCreator) CustomJson(_ []string, _ []string, _ string, _ string) hivego.HiveOperation {
	return nil
}
func (stubHiveCreator) Transfer(_ string, _ string, _ string, _ string, _ string) hivego.HiveOperation {
	return nil
}
func (stubHiveCreator) TransferToSavings(_ string, _ string, _ string, _ string, _ string) hivego.HiveOperation {
	return nil
}
func (stubHiveCreator) TransferFromSavings(_ string, _ string, _ string, _ string, _ string, _ int) hivego.HiveOperation {
	return nil
}
func (stubHiveCreator) UpdateAccount(_ string, _ *hivego.Auths, _ *hivego.Auths, _ *hivego.Auths, _ string, _ string) hivego.HiveOperation {
	return nil
}
func (stubHiveCreator) MakeTransaction(_ []hivego.HiveOperation) hivego.HiveTransaction {
	return hivego.HiveTransaction{}
}
func (stubHiveCreator) PopulateSigningProps(_ *hivego.HiveTransaction, _ []int) error { return nil }
func (stubHiveCreator) Sign(_ hivego.HiveTransaction) (string, error)                { return "sig", nil }
func (stubHiveCreator) Broadcast(_ hivego.HiveTransaction) (string, error)           { return "txid", nil }

// TestExecuteActionsDoesNotReselectBroadcastActions reproduces CRITICAL #1
// (gateway double-spend). A single pending withdraw is selected on the first
// action tick and broadcast. With no intervening L1 `vsc.actions` header
// ingest, the *next* action tick must NOT re-select and re-pay the same
// withdrawal.
func TestExecuteActionsDoesNotReselectBroadcastActions(t *testing.T) {
	const bh = uint64(20) // ACTION_INTERVAL, so bh % ACTION_INTERVAL == 0
	const actionID = "withdraw-1"

	actionsDb := &test_utils.MockActionsDb{
		Actions: map[string]ledgerDb.ActionRecord{
			actionID: {
				Id:          actionID,
				Status:      "pending",
				Amount:      1000,
				Asset:       "hbd",
				To:          "hive:bob",
				Type:        "withdraw",
				BlockHeight: 1,
			},
		},
	}

	ms := &MultiSig{
		sconf:         systemconfig.MocknetConfig(),
		ledgerActions: actionsDb,
		hiveCreator:   stubHiveCreator{},
	}

	// First action tick: the withdraw is pending, so it is selected & broadcast.
	pkg1, err := ms.executeActions(bh)
	if err != nil {
		t.Fatalf("first executeActions: unexpected error: %v", err)
	}
	if len(pkg1.Ops) < 2 {
		t.Fatalf("first executeActions: expected header + transfer op, got %d ops", len(pkg1.Ops))
	}

	// The selected action must no longer be re-selectable: a broadcast batch
	// is in-flight until its L1 header confirms.
	rec, _ := actionsDb.Get(actionID)
	if rec.Status == "pending" {
		t.Fatalf("action %s still 'pending' after being broadcast — next tick will double-pay it", actionID)
	}

	// Second action tick, no L1 ingest in between: nothing should be selected.
	_, err = ms.executeActions(bh)
	if err == nil {
		t.Fatalf("second executeActions: re-selected an already-broadcast action (double-spend)")
	}
	if err.Error() != "no actions to process" {
		t.Fatalf("second executeActions: expected \"no actions to process\", got %v", err)
	}
}
