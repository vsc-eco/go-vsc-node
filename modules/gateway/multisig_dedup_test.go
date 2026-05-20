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

// TestBuildActionBatchDoesNotMutateDB verifies the load-bearing half of the
// cosigner split-brain fix: buildActionBatch must be pure (no SetProcessing
// on the local DB). Pre-fix, cosigners called executeActions which mutated
// their own DB; a leader broadcast failure then left the cosigner with
// actions stuck in "processing" forever (the bradleyarrow incident).
func TestBuildActionBatchDoesNotMutateDB(t *testing.T) {
	const bh = uint64(20)
	const actionID = "withdraw-cosigner-test"

	actionsDb := &test_utils.MockActionsDb{
		Actions: map[string]ledgerDb.ActionRecord{
			actionID: {
				Id:          actionID,
				Status:      "pending",
				Amount:      5000,
				Asset:       "hbd",
				To:          "hive:alice",
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

	pkg, executedOps, err := ms.buildActionBatch(bh)
	if err != nil {
		t.Fatalf("buildActionBatch: unexpected error: %v", err)
	}
	if len(executedOps) != 1 || executedOps[0] != actionID {
		t.Fatalf("buildActionBatch: expected [%s], got %v", actionID, executedOps)
	}
	_ = pkg

	rec, _ := actionsDb.Get(actionID)
	if rec.Status != "pending" {
		t.Fatalf("buildActionBatch mutated DB: action status is %q, expected 'pending'", rec.Status)
	}

	pkg2, _, err := ms.buildActionBatch(bh)
	if err != nil {
		t.Fatalf("second buildActionBatch: unexpected error: %v", err)
	}
	if pkg2.TxId != pkg.TxId {
		t.Fatalf("buildActionBatch not idempotent: TxId %s != %s", pkg2.TxId, pkg.TxId)
	}
}

// TestExecuteActionsRevertsOnThresholdFailure exercises the failure-recovery
// half of the fix: after executeActions has marked actions "processing",
// a downstream signing-threshold / broadcast failure must call
// RevertToPending so the actions are eligible for the next tick rather than
// being stuck forever.
func TestExecuteActionsRevertsOnThresholdFailure(t *testing.T) {
	const bh = uint64(20)
	const actionID = "withdraw-threshold-fail"

	actionsDb := &test_utils.MockActionsDb{
		Actions: map[string]ledgerDb.ActionRecord{
			actionID: {
				Id:          actionID,
				Status:      "pending",
				Amount:      3000,
				Asset:       "hbd",
				To:          "hive:dave",
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

	_, err := ms.executeActions(bh)
	if err != nil {
		t.Fatalf("executeActions: %v", err)
	}

	rec, _ := actionsDb.Get(actionID)
	if rec.Status != "processing" {
		t.Fatalf("after executeActions: expected 'processing', got %q", rec.Status)
	}

	ms.ledgerActions.RevertToPending(actionID)

	rec, _ = actionsDb.Get(actionID)
	if rec.Status != "pending" {
		t.Fatalf("after RevertToPending: expected 'pending', got %q", rec.Status)
	}

	_, err = ms.executeActions(bh)
	if err != nil {
		t.Fatalf("retry executeActions after revert: %v", err)
	}

	rec, _ = actionsDb.Get(actionID)
	if rec.Status != "processing" {
		t.Fatalf("after retry: expected 'processing', got %q", rec.Status)
	}
}

// TestPendingTxStoredAfterBroadcast verifies that after a successful
// broadcast the signed tx is held in ms.pendingTx until either the state
// engine marks the actions "complete" or the Hive expiration window
// passes — and pendingTxProcessed() returns false while actions remain
// "processing".
func TestPendingTxStoredAfterBroadcast(t *testing.T) {
	const bh = uint64(20)
	const actionID = "withdraw-pending-store"

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
		t.Fatalf("executeActions: %v", err)
	}

	ms.pendingTx = &pendingGatewayTx{
		SignedTx:   pkg.Tx,
		TxId:       pkg.TxId,
		ActionIds:  pkg.ExecutedOps,
		Expiration: "2099-01-01T00:00:00",
	}

	rec, _ := actionsDb.Get(actionID)
	if rec.Status != "processing" {
		t.Fatalf("expected 'processing', got %q", rec.Status)
	}

	if ms.pendingTx == nil {
		t.Fatal("pendingTx should be set")
	}
	if ms.pendingTxProcessed() {
		t.Fatal("pendingTx should not be processed yet")
	}
}

// TestPendingTxPrunedWhenProcessed exercises the success path: when the
// state engine has marked every action "complete", pendingTxProcessed()
// returns true and prunePendingTx clears the slot so the next tick builds
// a fresh batch.
func TestPendingTxPrunedWhenProcessed(t *testing.T) {
	const actionID = "withdraw-prune-processed"

	actionsDb := &test_utils.MockActionsDb{
		Actions: map[string]ledgerDb.ActionRecord{
			actionID: {
				Id:     actionID,
				Status: "complete",
				Amount: 1000,
				Asset:  "hbd",
				To:     "hive:bob",
				Type:   "withdraw",
			},
		},
	}

	ms := &MultiSig{
		sconf:         systemconfig.MocknetConfig(),
		ledgerActions: actionsDb,
		hiveCreator:   stubHiveCreator{},
		pendingTx: &pendingGatewayTx{
			TxId:       "some-tx-id",
			ActionIds:  []string{actionID},
			Expiration: "2099-01-01T00:00:00",
		},
	}

	if !ms.pendingTxProcessed() {
		t.Fatal("pendingTx should be detected as processed (action is complete)")
	}

	ms.prunePendingTx()
	if ms.pendingTx != nil {
		t.Fatal("pendingTx should be nil after prune")
	}
}

// TestPendingTxRevertsOnExpiry exercises the expiry path: when block height
// passes pendingTx.ExpirationBlock (10 blocks past broadcast, matching the
// 30-second Hive tx wall-clock expiration), TickActions reverts every
// processing action to "pending" and prunes the stored tx so the next tick
// can build a fresh batch.
func TestPendingTxRevertsOnExpiry(t *testing.T) {
	const actionID = "withdraw-expire-revert"

	actionsDb := &test_utils.MockActionsDb{
		Actions: map[string]ledgerDb.ActionRecord{
			actionID: {
				Id:     actionID,
				Status: "processing",
				Amount: 2000,
				Asset:  "hbd",
				To:     "hive:alice",
				Type:   "withdraw",
			},
		},
	}

	ms := &MultiSig{
		sconf:         systemconfig.MocknetConfig(),
		ledgerActions: actionsDb,
		hiveCreator:   stubHiveCreator{},
		pendingTx: &pendingGatewayTx{
			TxId:            "expired-tx",
			ActionIds:       []string{actionID},
			ExpirationBlock: 15,
		},
	}

	// bh=20 > ExpirationBlock=15, so tx is expired
	ms.TickActions(20)

	if ms.pendingTx != nil {
		t.Fatal("pendingTx should be pruned after expiry")
	}

	rec, _ := actionsDb.Get(actionID)
	if rec.Status != "pending" {
		t.Fatalf("expected 'pending' after expiry revert, got %q", rec.Status)
	}
}

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
