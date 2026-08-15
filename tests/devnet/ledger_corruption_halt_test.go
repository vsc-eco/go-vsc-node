package devnet

import (
	"context"
	"testing"
	"time"

	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestLedgerCorruptionHaltGuard is the end-to-end validation of the
// negative-spendable-balance fail-stop (UpdateBalances in state-processing).
//
// It reproduces the 2026-08 mainnet halt in miniature: one node holds a
// stored balance a few units below its peers, and the account then performs a
// zero-margin "max" operation (unstaking its exact full hbd_savings). On the
// healthy majority the unstake succeeds and the block finalizes; the corrupted
// node applies that finalized debit verbatim and would land on a NEGATIVE
// balance. The guard turns that silent corruption into a clean, loud halt.
//
// Expected outcome WITH the guard: the corrupted node stops advancing
// last_processed_block (its block pipeline panics/halts), while the healthy
// nodes keep finalizing. WITHOUT the guard the corrupted node would instead
// write the negative balance and keep going (the cascade this fix prevents),
// so this test fails on a pre-guard binary.
//
// NOTE: not run in CI here (heavy multi-node docker). Written for
// `make test-regression`-style execution. Review the two marked assumptions
// (the corrupted account name + that no in-memory balance cache shadows the
// Mongo write) against the running harness before relying on it.
func TestLedgerCorruptionHaltGuard(t *testing.T) {
	requireDocker(t)

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.SkipFunding = false // we need real funds to deposit + stake savings
	cfg.LogLevel = "error"
	d, ctx := startDevnet(t, cfg, 20*time.Minute)

	const (
		staker      = 1        // witness whose account we stake + corrupt
		corruptNode = 5        // the node we corrupt (a minority of 5)
		stakeAmt    = "5.000"  // → hbd_savings 5000 (milli-units)
		unstakeAmt  = "5.000"  // exact full balance → zero margin
		corruptBy   = int64(1) // stored balance 1 unit low on corruptNode
	)
	// The ledger keys accounts hive:-qualified, and isGuardedAccount scopes the
	// guard to exactly that prefix. witnessAccount returns the bare name
	// ("magi.test1"), so without this the corruption UpdateOne matches zero
	// documents and the guard is never exercised. Every other devnet test
	// does the same qualification (blame_cycle, bond_gate_gap1, pr181, ...).
	account := "hive:" + d.witnessAccount(staker)

	// 1. Move funds into hbd_savings and let the stake finalize network-wide.
	if _, err := d.Deposit(ctx, staker, "20.000", "hbd"); err != nil {
		t.Fatalf("deposit hbd: %v", err)
	}
	if _, err := d.StakeHBD(staker, staker, stakeAmt); err != nil {
		t.Fatalf("stake hbd: %v", err)
	}

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(d.MongoURI()))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	defer client.Disconnect(context.Background())

	// Wait until the corrupt node has materialized the staked balance so there
	// is a real snapshot row to tamper with.
	corruptColl := client.Database(d.nodeDbName(corruptNode)).Collection("ledger_balances")
	var snap ledgerDb.BalanceRecord
	// 2 minutes is not enough on a fresh devnet. StakeHBD debits hbd immediately,
	// but the hbd_savings CREDIT lands via IndexActions, driven by the gateway's
	// vsc.actions batch — emitted every ACTION_INTERVAL (20) blocks and requiring
	// 2/3 multisig cosigning. Right after the genesis election the committee is
	// still coming up, so the credit arrives minutes later. Observed failing at
	// 2m with HBD:15000 (debit applied) / HBD_SAVINGS:0 (credit pending).
	deadline := time.Now().Add(12 * time.Minute)
	for {
		opts := options.FindOne().SetSort(bson.D{{Key: "block_height", Value: -1}})
		err := corruptColl.FindOne(ctx, bson.M{"account": account}, opts).Decode(&snap)
		if err == nil && snap.HBD_SAVINGS >= 5000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("staked balance never materialized on node %d (last=%+v, err=%v)", corruptNode, snap, err)
		}
		time.Sleep(2 * time.Second)
	}

	// 2. Inject the corruption: drop hbd_savings by corruptBy on the corrupt
	//    node's latest snapshot only. Now node %d holds 4999 where its peers
	//    hold 5000. (No in-memory balance cache shadows this — see ASSUMPTION.)
	res, err := corruptColl.UpdateOne(ctx,
		bson.M{"account": account, "block_height": snap.BlockHeight},
		bson.M{"$inc": bson.M{"hbd_savings": -corruptBy}},
	)
	if err != nil || res.ModifiedCount != 1 {
		t.Fatalf("inject corruption failed: modified=%d err=%v", res.ModifiedCount, err)
	}

	// Baseline heights just before the poison op.
	baseCorrupt, err := d.getLastProcessedBlock(ctx, corruptNode)
	if err != nil {
		t.Fatalf("read corrupt-node height: %v", err)
	}

	// 3. The zero-margin max unstake. On the healthy majority (4/5) balance is
	//    5000, so the unstake succeeds and the block finalizes.
	if _, err := d.UnstakeHBD(staker, staker, unstakeAmt); err != nil {
		t.Fatalf("unstake hbd: %v", err)
	}

	// 4. Healthy nodes must keep finalizing past the unstake.
	for _, n := range []int{1, 2, 3, 4} {
		if err := d.WaitForBlockProcessing(ctx, n, baseCorrupt+20, 3*time.Minute); err != nil {
			t.Fatalf("healthy node %d did not advance past the unstake: %v", n, err)
		}
	}

	// 5. The corrupted node must HALT: its last_processed_block stops advancing
	//    once it applies the finalized debit that would drive hbd_savings < 0.
	//    Poll for a stall: allow a short grace, then require it not to have moved
	//    meaningfully while the healthy nodes ran well ahead.
	time.Sleep(90 * time.Second)
	haltedAt, err := d.getLastProcessedBlock(ctx, corruptNode)
	if err != nil {
		// Deliberately fatal, not tolerated. The original form logged this and
		// returned PASS on the reasoning that a hard-stopped node may fail its
		// own height read -- but that premise does not hold here: mongoClient
		// connects to a SINGLE mongo server and nodeDbName only selects database
		// "magi-N", so the store is a separate container whose readability is
		// independent of node liveness. Verified on the 2026-08-15 run: the
		// corrupt node's container was Exited(1) and this read still returned
		// 299 normally.
		//
		// Tolerating it would let an infrastructure hiccup score as a successful
		// halt with nothing actually inspected -- the assertion below never runs
		// and the guard is never checked. Note the read for the healthy node
		// immediately after is already t.Fatalf on the same condition; this
		// makes the two consistent.
		t.Fatalf("read corrupt-node %d height: %v", corruptNode, err)
	}
	healthy, err := d.getLastProcessedBlock(ctx, 1)
	if err != nil {
		t.Fatalf("read healthy-node height: %v", err)
	}
	if haltedAt >= healthy-5 {
		t.Fatalf("corrupt node %d did NOT halt: advanced to %d alongside healthy %d — the guard did not fire (negative balance was persisted instead)",
			corruptNode, haltedAt, healthy)
	}

	// 6. And it must NOT have persisted a negative balance (the whole point).
	var post ledgerDb.BalanceRecord
	opts := options.FindOne().SetSort(bson.D{{Key: "block_height", Value: -1}})
	if err := corruptColl.FindOne(ctx, bson.M{"account": account}, opts).Decode(&post); err == nil {
		if post.HBD_SAVINGS < 0 {
			t.Fatalf("corrupt node persisted a NEGATIVE hbd_savings (%d) — guard must halt BEFORE writing it", post.HBD_SAVINGS)
		}
	}

	t.Logf("guard OK: node %d halted at %d while healthy node reached %d, no negative balance written",
		corruptNode, haltedAt, healthy)
}
