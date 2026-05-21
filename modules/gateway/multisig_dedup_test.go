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

// TestExecuteActionsDoesNotMutateDB guards the cosigner split-brain fix.
// executeActions is called by BOTH the action leader (TickActions) and every
// cosigner (p2p HandleMessage on a sign_request), so it must be PURE: it builds
// the deterministic batch but must not mutate local action state. The earlier
// "processing" intermediate state (c9a86798) violated this — a cosigner marked
// the selected batch "processing" in its own DB, and if the leader's broadcast
// then failed the cosigner was left with actions stuck "processing" forever
// (never re-selected, never completed: the bradleyarrow incident).
//
// Re-selection safety does NOT depend on a per-node status flag: it comes from
// L1 settlement (status -> "complete" when the vsc.actions header is re-ingested)
// plus the ACTION_INTERVAL (20 blocks) > tx-expiry (~10 blocks) timing, so by the
// next selection tick a prior attempt has either settled (and is excluded) or
// permanently expired.
func TestExecuteActionsDoesNotMutateDB(t *testing.T) {
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

	pkg, err := ms.executeActions(bh)
	if err != nil {
		t.Fatalf("executeActions: unexpected error: %v", err)
	}
	if len(pkg.Ops) < 2 {
		t.Fatalf("executeActions: expected header + transfer op, got %d ops", len(pkg.Ops))
	}

	// The action must be untouched — a cosigner running this must not poison its
	// own action state.
	rec, _ := actionsDb.Get(actionID)
	if rec.Status != "pending" {
		t.Fatalf("executeActions mutated DB: action status is %q, expected 'pending'", rec.Status)
	}
}
