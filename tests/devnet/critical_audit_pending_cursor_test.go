package devnet

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestCriticalAuditPendingActionsCursor verifies the fix from
// 8130b95e "pendulum critical-audit phase 1": the cursor loop in
// actionsDb.GetPendingActionsByEpoch.
//
// Bug: the prior form used `if cursor.Next(...)` so the function
// returned at most ONE pending action per call:
//
//	for cursor.Next(...) {   // <-- was: if cursor.Next(...)
//	    record := ActionRecord{}
//	    cursor.Decode(&record)
//	    actionRecords = append(actionRecords, record)
//	}
//
// The caller (state_engine.UpdateBalances) processes the slice in
// one slot: it adds every entry to `completeIds`, writes one
// LedgerRecord per entry with id="<actionId>#out" and
// BlockHeight=endBlock, then ExecuteCompletes the lot. Under the
// bug, only one of N pending unstakes for the same epoch could be
// released per slot — the rest had to wait for subsequent slots,
// where the same single-record query would dequeue another. That
// staggered the release of unstakes that should have been atomic
// at the epoch boundary, with two visible consequences:
//
//  1. The `#out` LedgerRecords for sibling unstakes land at
//     different BlockHeights (different slots) rather than the same
//     slot's `endBlock`.
//  2. In the worst case the bug interacts with epoch transitions —
//     if a new election lands between two slot-releases, the second
//     unstake is now keyed off a different election epoch.
//
// This test detects (1): if both sibling unstakes are submitted
// before the same election and queued for the same release epoch,
// their `#out` ledger records must land at IDENTICAL BlockHeights
// post-fix. Under the bug they'd differ by at least one slot.
//
// Test flow:
//
//  1. Spin up devnet.
//  2. Wait until at least one running election has landed (epoch >= 1).
//  3. Submit two consensus_unstake ops for the same target account
//     in quick succession (both should be processed under the same
//     election, so both get `data.epoch = currentEpoch + 5`).
//  4. Wait for both to appear in ledger_actions with status="pending"
//     and capture their action IDs.
//  5. Verify both share the same `data.epoch` — otherwise the test
//     setup didn't actually queue siblings; retry would be needed.
//  6. Wait for the release boundary plus generous slack.
//  7. Verify both actions have status="complete".
//  8. Look up each action's `#out` LedgerRecord in the `ledger`
//     collection and assert they have the same BlockHeight.
//
// If step 8 sees different BlockHeights, the fix is not in effect.
func TestCriticalAuditPendingActionsCursor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnetNoKey(t, cfg, 20*time.Minute)

	// Wait until every node has ingested a running election. The unstake
	// guard from PR #181 (#96) rejects pre-election unstakes, and we
	// want a clean post-election baseline anyway.
	t.Log("waiting for first running election (epoch >= 1) on every node...")
	for n := 1; n <= cfg.Nodes; n++ {
		nodeCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		if err := d.waitForElectionEpoch(nodeCtx, n, 1, 8*time.Minute); err != nil {
			cancel()
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
		cancel()
	}

	// Pick a non-genesis witness to unstake from.
	witnessIdx := 1
	if cfg.GenesisNode == 1 {
		witnessIdx = 2
	}
	targetAccount := fmt.Sprintf("%s%d", cfg.WitnessPrefix, witnessIdx)
	hiveTarget := "hive:" + targetAccount

	// Submit two sibling unstakes in quick succession. Hive block
	// production is ~3s so both should land in the same L1 block or
	// adjacent ones — well within the same election epoch.
	t.Logf("submitting unstake A for %s", targetAccount)
	if err := d.Unstake(ctx, targetAccount, "1.000"); err != nil {
		t.Fatalf("broadcasting unstake A: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	t.Logf("submitting unstake B for %s", targetAccount)
	if err := d.Unstake(ctx, targetAccount, "1.000"); err != nil {
		t.Fatalf("broadcasting unstake B: %v", err)
	}

	// Wait until both unstakes have landed as pending in ledger_actions
	// on magi-1, and capture their {id, releaseEpoch} pairs.
	t.Log("waiting for both unstakes to land as pending in ledger_actions...")
	siblings, err := waitForTwoPendingUnstakes(ctx, d, 1, hiveTarget, 3*time.Minute)
	if err != nil {
		t.Fatalf("waiting for sibling unstakes: %v", err)
	}
	t.Logf("captured sibling action ids: %s (epoch %d) and %s (epoch %d)",
		siblings[0].Id, siblings[0].ReleaseEpoch,
		siblings[1].Id, siblings[1].ReleaseEpoch)

	// Sanity: both must release on the same epoch boundary. Otherwise
	// the test setup didn't actually queue siblings (e.g. an election
	// landed between the two submissions), and the bug-detection
	// assertion below wouldn't be meaningful.
	if siblings[0].ReleaseEpoch != siblings[1].ReleaseEpoch {
		t.Skipf("sibling unstakes ended up on different release epochs (%d vs %d); election landed between submissions — retry would be needed",
			siblings[0].ReleaseEpoch, siblings[1].ReleaseEpoch)
	}

	releaseEpoch := siblings[0].ReleaseEpoch
	t.Logf("both siblings queued for release at epoch %d", releaseEpoch)

	// Wait until magi-1 has ingested an election with epoch >= releaseEpoch,
	// then give state-engine a few extra slots to actually process the
	// release. UpdateBalances runs every slot, so 4 slots is generous.
	t.Logf("waiting for release epoch %d on magi-1...", releaseEpoch)
	if err := d.waitForElectionEpoch(ctx, 1, releaseEpoch, 12*time.Minute); err != nil {
		t.Fatalf("magi-1 never ingested release epoch %d: %v", releaseEpoch, err)
	}
	time.Sleep(45 * time.Second)

	// Both action records should now be status="complete" on every node.
	for n := 1; n <= cfg.Nodes; n++ {
		for _, sib := range siblings {
			rec, err := readActionById(ctx, d, n, sib.Id)
			if err != nil {
				t.Errorf("magi-%d: failed reading action %s: %v", n, sib.Id, err)
				continue
			}
			if rec.Status != "complete" {
				t.Errorf("magi-%d: action %s status=%q (expected complete) — release may have been deferred", n, sib.Id, rec.Status)
			}
		}
	}

	// Core fix assertion: both #out ledger records must share the same
	// BlockHeight. Under the bug, the cursor returned only the first
	// action per slot, so the second sibling's #out record would have
	// been written at a later endBlock (next slot).
	for n := 1; n <= cfg.Nodes; n++ {
		bhA, errA := readOutLedgerBlockHeight(ctx, d, n, siblings[0].Id)
		bhB, errB := readOutLedgerBlockHeight(ctx, d, n, siblings[1].Id)
		if errA != nil || errB != nil {
			t.Errorf("magi-%d: failed reading #out records (errA=%v errB=%v)", n, errA, errB)
			continue
		}
		if bhA != bhB {
			t.Errorf("magi-%d: sibling unstakes released at different BlockHeights (A=%d, B=%d) — cursor-loop fix not in effect",
				n, bhA, bhB)
		} else {
			t.Logf("magi-%d: both siblings released at BlockHeight %d (atomic — fix in effect)", n, bhA)
		}
	}
}

type pendingUnstakeRecord struct {
	Id           string
	ReleaseEpoch uint64
}

// waitForTwoPendingUnstakes polls magi-N's ledger_actions for two
// status=pending records of type unstake matching the target hive
// account. Returns when at least 2 are found (the most recent two by
// block_height ascending).
func waitForTwoPendingUnstakes(ctx context.Context, d *Devnet, node int, hiveTarget string, timeout time.Duration) ([]pendingUnstakeRecord, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		recs, err := findPendingUnstakes(ctx, d, node, hiveTarget)
		if err == nil && len(recs) >= 2 {
			return recs[:2], nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return nil, fmt.Errorf("timed out waiting for two pending unstakes on magi-%d for %s", node, hiveTarget)
}

func findPendingUnstakes(ctx context.Context, d *Devnet, node int, hiveTarget string) ([]pendingUnstakeRecord, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("ledger_actions")
	queries := []bson.M{
		{"status": "pending", "type": bson.M{"$regex": "unstake"}, "to": hiveTarget},
		{"status": "pending", "type": bson.M{"$regex": "unstake"}, "from": hiveTarget},
		{"status": "pending", "type": bson.M{"$regex": "unstake"}, "data.from": hiveTarget},
	}
	seen := map[string]pendingUnstakeRecord{}
	for _, q := range queries {
		cur, err := coll.Find(ctx, q, options.Find().SetSort(bson.D{{Key: "block_height", Value: 1}}))
		if err != nil {
			return nil, err
		}
		for cur.Next(ctx) {
			var raw struct {
				Id   string `bson:"id"`
				Data struct {
					Epoch uint64 `bson:"epoch"`
				} `bson:"data"`
			}
			if err := cur.Decode(&raw); err != nil {
				cur.Close(ctx)
				return nil, err
			}
			seen[raw.Id] = pendingUnstakeRecord{Id: raw.Id, ReleaseEpoch: raw.Data.Epoch}
		}
		cur.Close(ctx)
	}
	out := make([]pendingUnstakeRecord, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	return out, nil
}

type actionStatus struct {
	Id     string `bson:"id"`
	Status string `bson:"status"`
}

func readActionById(ctx context.Context, d *Devnet, node int, id string) (actionStatus, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return actionStatus{}, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("ledger_actions")
	var rec actionStatus
	if err := coll.FindOne(ctx, bson.M{"id": id}).Decode(&rec); err != nil {
		return actionStatus{}, err
	}
	return rec, nil
}

// readOutLedgerBlockHeight finds the LedgerRecord written when a
// consensus_unstake action releases — its id is "<actionId>#out". The
// BlockHeight on that record is the slot endBlock at which the
// release was processed.
func readOutLedgerBlockHeight(ctx context.Context, d *Devnet, node int, actionId string) (uint64, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("ledger")
	var raw struct {
		Id          string `bson:"id"`
		BlockHeight uint64 `bson:"block_height"`
	}
	if err := coll.FindOne(ctx, bson.M{"id": actionId + "#out"}).Decode(&raw); err != nil {
		return 0, fmt.Errorf("magi-%d: ledger record %s#out not found: %w", node, actionId, err)
	}
	return raw.BlockHeight, nil
}
