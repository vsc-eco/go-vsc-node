package devnet

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestPR181ConsensusUnstakeRejectedBeforeFirstElection verifies the
// fix from PR #181 commit 30848bbd (review4 HIGH #96).
//
// Bug:
//
//	electionResult := se.GetElectionInfo(tx.Self.BlockHeight - 1)
//	params := ledgerSystem.ConsensusParams{
//	    ...
//	    ElectionEpoch: electionResult.Epoch + 5,  // <-- zero on failure
//	}
//
// GetElectionInfo swallows the underlying DB error and returns a
// zero-value ElectionResult. An unstake processed in a window where
// the election lookup yields Epoch=0 would then lock for "5 epochs
// from epoch 0" — i.e. epoch 5 — regardless of the current real
// epoch. At any current epoch > 5 the lock is already expired and
// the unstake auto-unlocks immediately (or never locks at all).
//
// Fix:
//
//	if electionResult.Epoch == 0 && tx.Self.BlockHeight > 1 {
//	    return TxResult{
//	        Success: false,
//	        Ret:     "election lookup unavailable; retry unstake later",
//	        RcUsed:  50,
//	    }
//	}
//
// The fix refuses the tx so the user resubmits once the lookup
// recovers, instead of locking under a stale zero epoch.
//
// Devnet-exercisable window:
//
//   - genesis-elector produces the genesis election with Epoch=0 at
//     boot.
//   - The running election proposer first emits a real election at
//     block ~ElectionInterval (60s after boot in this config).
//   - Any consensus_unstake whose containing L1 block is ingested by
//     magid in that pre-election window has
//     `electionResult.Epoch == 0 && BlockHeight > 1` — the exact guard
//     condition.
//
// Test:
//
//  1. Spins up devnet (no TSS key needed — this exercises the state
//     engine, not TSS).
//  2. Submits a consensus_unstake immediately, in the pre-election
//     window.
//  3. Waits until every node has ingested epoch >= 1.
//  4. Submits a second consensus_unstake post-election.
//  5. Waits a few blocks for both to settle.
//  6. Queries each node's `ledger_actions` collection for the target
//     account. We expect exactly ONE action of type "unstake": the
//     post-election one. The pre-election one must NOT have produced
//     an action record (the guard rejected it).
//
// Failure-mode honesty: this test cannot specifically inject a
// transient Mongo failure on `GetElectionByHeight` — that's the
// underlying root-cause path the comment describes. It exploits the
// natural boot-window where electionResult.Epoch==0 is the only state
// the lookup can return. Together with the unit tests of the new code
// path, this gives end-to-end evidence the guard rejects in a real
// devnet exactly when it should.
func TestPR181ConsensusUnstakeRejectedBeforeFirstElection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnetNoKey(t, cfg, 15*time.Minute)

	// Use a non-genesis witness so we don't interfere with block production.
	// WitnessPrefix defaults to "magi.test" -> "magi.test1" etc.
	witnessIdx := 1
	if cfg.GenesisNode == 1 {
		witnessIdx = 2
	}
	targetAccount := fmt.Sprintf("%s%d", cfg.WitnessPrefix, witnessIdx)

	// Submit the early unstake immediately — racing against the running
	// election proposer's first epoch (~ElectionInterval blocks away). We
	// expect this to land in an L1 block whose ingested BlockHeight on
	// magid is > 1 but before any post-genesis election has been
	// processed.
	earlyAmount := "1.000"
	t.Logf("submitting early consensus_unstake for %s (%s) in pre-election window", targetAccount, earlyAmount)
	if err := d.Unstake(ctx, targetAccount, earlyAmount); err != nil {
		t.Fatalf("broadcasting early unstake: %v", err)
	}

	// Wait for the first running election (epoch >= 1) on every node, so
	// we know the pre-election window has definitively closed.
	t.Log("waiting for first running election (epoch >= 1) on every node...")
	for n := 1; n <= cfg.Nodes; n++ {
		nodeCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		if err := d.waitForElectionEpoch(nodeCtx, n, 1, 8*time.Minute); err != nil {
			cancel()
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
		cancel()
	}

	// Give magid extra blocks to fully process the early unstake either
	// way (recorded as rejected, or accepted under the buggy code path).
	time.Sleep(15 * time.Second)

	// Snapshot how many unstake actions exist after the pre-election
	// window. Under the fix this should be 0; under the bug it would be 1.
	earlyCounts := make([]int64, cfg.Nodes)
	for n := 1; n <= cfg.Nodes; n++ {
		c, err := countUnstakeActions(ctx, d, n, targetAccount)
		if err != nil {
			t.Fatalf("counting early unstake actions on magi-%d: %v", n, err)
		}
		earlyCounts[n-1] = c
		t.Logf("magi-%d: %d unstake action(s) recorded after pre-election window (expect 0)", n, c)
	}

	// Now submit a second unstake — this one must succeed because epoch >= 1
	// is in place.
	laterAmount := "2.000"
	t.Logf("submitting later consensus_unstake for %s (%s) post-election", targetAccount, laterAmount)
	if err := d.Unstake(ctx, targetAccount, laterAmount); err != nil {
		t.Fatalf("broadcasting later unstake: %v", err)
	}

	// Wait a generous window for the later unstake's L1 op to be ingested,
	// processed, and recorded in ledger_actions.
	time.Sleep(30 * time.Second)

	// Verify post-state: each node should have exactly one MORE unstake
	// action than it had after the pre-election window — the late one.
	for n := 1; n <= cfg.Nodes; n++ {
		c, err := countUnstakeActions(ctx, d, n, targetAccount)
		if err != nil {
			t.Fatalf("counting final unstake actions on magi-%d: %v", n, err)
		}
		expected := earlyCounts[n-1] + 1
		if c != expected {
			t.Errorf("magi-%d: expected %d unstake actions after post-election unstake, got %d", n, expected, c)
		}
	}

	// Final guard: ensure earlyCounts was 0 everywhere. If it was >= 1, the
	// early unstake was NOT rejected — the fix is not in effect on this
	// node. We assert this AFTER the late-unstake step so we still get the
	// "+1 happens correctly" signal as a sanity check on the happy path.
	for n := 1; n <= cfg.Nodes; n++ {
		if earlyCounts[n-1] != 0 {
			t.Errorf("magi-%d: %d unstake action(s) recorded from the pre-election submission — guard did not reject", n, earlyCounts[n-1])
		}
	}
}

// countUnstakeActions returns the number of `ledger_actions` records on
// the given node whose `type` indicates consensus_unstake and whose
// owning account matches `account`. The exact bson layout is:
//
//	{ id: ..., type: "unstake" or contains "unstake", to/from: "hive:<account>", ... }
//
// We filter loosely on `type` containing "unstake" to be robust to small
// schema tweaks (e.g. "consensus_unstake" vs "unstake").
func countUnstakeActions(ctx context.Context, d *Devnet, node int, account string) (int64, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("ledger_actions")
	hiveAcc := "hive:" + account

	// Try multiple plausible filters; the action record may key the account
	// on `to`, `from`, or carry it in the `data` subdoc. Count the union.
	queries := []bson.M{
		{"type": bson.M{"$regex": "unstake"}, "to": hiveAcc},
		{"type": bson.M{"$regex": "unstake"}, "from": hiveAcc},
		{"type": bson.M{"$regex": "unstake"}, "data.from": hiveAcc},
	}
	seen := map[string]struct{}{}
	for _, q := range queries {
		cur, err := coll.Find(ctx, q, options.Find().SetProjection(bson.M{"id": 1}))
		if err != nil {
			return 0, fmt.Errorf("find on magi-%d: %w", node, err)
		}
		for cur.Next(ctx) {
			var doc struct {
				Id string `bson:"id"`
			}
			if err := cur.Decode(&doc); err != nil {
				cur.Close(ctx)
				return 0, fmt.Errorf("decode on magi-%d: %w", node, err)
			}
			seen[doc.Id] = struct{}{}
		}
		cur.Close(ctx)
	}
	return int64(len(seen)), nil
}

// silence the unused-import linter if mongo isn't referenced directly elsewhere
var _ = mongo.ErrNoDocuments
