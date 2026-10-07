package devnet

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"go.mongodb.org/mongo-driver/bson"
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
// Superseded 2026-05-22/26 (f935ac6e, 120b765c): the read is now the
// fail-stop GetElectionInfoOrBlock. A read error blocks instead of becoming
// epoch 0, so the bug cannot happen, and the genesis election (epoch 0) is a
// real election: an unstake under it is accepted and locked to epoch
// 0 + CONSENSUS_UNSTAKE_LOCK_EPOCHS. The test now checks that invariant on
// every recorded unstake (lock epoch = the epoch in force at its block + the
// lock period), which the original bug, a zero epoch at a later epoch,
// breaks. From 0.7 the POA exit-halt also refuses an unstake by an electable
// seat, so the post-election unstake is refused there.
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

	// Give magid extra blocks to process the early unstake.
	time.Sleep(15 * time.Second)

	// Under the genesis election the early unstake is accepted (see above).
	for n := 1; n <= cfg.Nodes; n++ {
		acts, err := unstakeActions(ctx, d, n, targetAccount)
		if err != nil {
			t.Fatalf("reading unstake actions on magi-%d: %v", n, err)
		}
		if len(acts) != 1 {
			t.Fatalf("PRECONDITION: magi-%d holds %d unstake actions after the early unstake, expected 1 (accepted under the genesis election); if 0, it landed after the first election", n, len(acts))
		}
		assertUnstakeLockEpoch(t, d, ctx, n, acts[0])
	}

	// A second unstake after the first election. From 0.7 the POA exit-halt
	// refuses it (magi.test1 is an electable committee seat); below 0.7 it is
	// accepted and locked like the first.
	laterAmount := "2.000"
	t.Logf("submitting later consensus_unstake for %s (%s) post-election", targetAccount, laterAmount)
	txL, err := d.ConsensusUnstake(witnessIdx, laterAmount)
	if err != nil {
		t.Fatalf("broadcasting later unstake: %v", err)
	}
	exitHalt := vfActiveConsensus(d, ctx) >= 7
	// Wait for the verdict rather than a fixed time: the action record is only
	// written when the L2 block closes, which under load took longer than 30 s.
	status := ""
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		status, _ = d.FindTransactionStatus(ctx, 1, txL)
		if status == "CONFIRMED" || status == "PROCESSED" || status == "FAILED" {
			break
		}
	}
	t.Logf("later unstake %s: %s (exit-halt in force: %v)", txL, status, exitHalt)
	want := 2
	if exitHalt {
		want = 1
		if status != "FAILED" {
			t.Errorf("later unstake %s by an electable seat is %q at >= 0.7, expected refused by the exit-halt", txL, status)
		}
	} else if status != "CONFIRMED" && status != "PROCESSED" {
		t.Errorf("later unstake %s is %q below 0.7, expected accepted", txL, status)
	}
	for n := 1; n <= cfg.Nodes; n++ {
		var acts []unstakeAction
		for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(3 * time.Second) {
			acts, err = unstakeActions(ctx, d, n, targetAccount)
			if err != nil {
				t.Fatalf("reading unstake actions on magi-%d: %v", n, err)
			}
			if len(acts) >= want || time.Now().After(deadline) {
				break
			}
		}
		if len(acts) != want {
			t.Errorf("magi-%d: %d unstake actions after the later unstake, expected %d (exit-halt in force: %v)", n, len(acts), want, exitHalt)
		}
		for _, a := range acts {
			assertUnstakeLockEpoch(t, d, ctx, n, a)
		}
	}
}

type unstakeAction struct {
	Id          string `bson:"id"`
	BlockHeight uint64 `bson:"block_height"`
	Data        struct {
		Epoch int64 `bson:"epoch"`
	} `bson:"data"`
}

// unstakeActions returns the consensus_unstake action records whose payout or
// bonded account is the given witness.
func unstakeActions(ctx context.Context, d *Devnet, node int, account string) ([]unstakeAction, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(ctx)
	coll := client.Database(d.nodeDbName(node)).Collection("ledger_actions")
	hiveAcc := "hive:" + account
	cur, err := coll.Find(ctx, bson.M{"type": "consensus_unstake", "$or": []bson.M{{"to": hiveAcc}, {"data.from": hiveAcc}, {"data.node": hiveAcc}}})
	if err != nil {
		return nil, fmt.Errorf("find on magi-%d: %w", node, err)
	}
	defer cur.Close(ctx)
	var out []unstakeAction
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("decode on magi-%d: %w", node, err)
	}
	return out, nil
}

// assertUnstakeLockEpoch: an unstake is locked to the epoch in force when it was
// processed plus CONSENSUS_UNSTAKE_LOCK_EPOCHS. The record's height is the end of
// its L2 block, which can sit just past an election, so the epoch one below is
// accepted too. The #96 bug (a failed read becoming epoch 0) gives a lock of 5 at
// any later epoch, far below both.
func assertUnstakeLockEpoch(t *testing.T, d *Devnet, ctx context.Context, node int, a unstakeAction) {
	t.Helper()
	var out struct {
		ElectionByBlockHeight struct {
			Epoch int64 `json:"epoch"`
		} `json:"electionByBlockHeight"`
	}
	q := fmt.Sprintf(`{ electionByBlockHeight(blockHeight: %d) { epoch } }`, a.BlockHeight)
	if err := d.gqlQuery(ctx, node, q, nil, &out); err != nil {
		t.Errorf("magi-%d: election at %d: %v", node, a.BlockHeight, err)
		return
	}
	lock := int64(params.CONSENSUS_UNSTAKE_LOCK_EPOCHS)
	at := out.ElectionByBlockHeight.Epoch
	if a.Data.Epoch != at+lock && a.Data.Epoch != at-1+lock {
		t.Errorf("magi-%d: unstake %s at block %d (epoch %d) is locked to epoch %d, expected %d", node, a.Id, a.BlockHeight, at, a.Data.Epoch, at+lock)
		return
	}
	t.Logf("magi-%d: unstake %s at block %d (epoch %d) locked to epoch %d", node, a.Id, a.BlockHeight, at, a.Data.Epoch)
}
