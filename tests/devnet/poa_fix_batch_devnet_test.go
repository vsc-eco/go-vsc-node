package devnet

import (
	"context"
	"slices"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// POA fix batch (0.9.0) devnet scenarios. They run at the 0.9.0 floor by default
// and pass; run against a tree without the fixes with POA_DEVNET_FLOOR=7 to see
// each finding reproduce.

func poaFixDevnet(t *testing.T, window uint64, fund bool, timeout time.Duration) (*Devnet, context.Context, uint64) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := tssTestConfig()
	if fund {
		cfg.SkipFunding = false
	}
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	floor := poaDevnetFloor()
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = floor
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1
	if window > 0 {
		cfg.SysConfigOverrides.ConsensusParams.PoaAdmitVoteWindowBlocks = window
	}
	d, ctx := startDevnetNoKey(t, cfg, timeout)
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 10*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	base, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("reading election epoch 2: %v", err)
	}
	if !allFlat(base.Weights) {
		t.Fatalf("PRECONDITION FAILED: epoch 2 weights=%v not flat; POA inert", base.Weights)
	}
	if s := pfMustSeats(t, d, ctx, 1); len(s) != cfg.Nodes {
		t.Fatalf("PRECONDITION FAILED: want %d bootstrap seats, got %d: %s", cfg.Nodes, len(s), pfSeatFingerprint(s))
	}
	return d, ctx, floor
}

func seated(t *testing.T, d *Devnet, ctx context.Context, account string) bool {
	for _, s := range pfMustSeats(t, d, ctx, 1) {
		if s.Account == account {
			return true
		}
	}
	return false
}

// POA-7: an admission proposal that expired must be able to run again for the
// same (candidate, owner) pair. Old rules: the pair is barred forever.
func TestPoaFixExpiredAdmissionReopens(t *testing.T) {
	const window = 60 // blocks (~3 min)
	d, ctx, floor := poaFixDevnet(t, window, false, 60*time.Minute)
	candidate, ubo := "magi.poa7cand", "ubo-poa7-001"

	// Seat 1 opens the proposal; nobody else votes inside the window.
	if _, err := d.pfAdmitVote(1, candidate, ubo); err != nil {
		t.Fatalf("opening vote: %v", err)
	}
	opened, err := d.getLastProcessedBlock(ctx, 1)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	for {
		h, err := d.getLastProcessedBlock(ctx, 1)
		if err == nil && h > opened+window+5 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting past the window: %v", ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}
	// A vote after the window: the old rules mark the proposal expired and drop
	// it; at 0.9.0 it starts round 2.
	if _, err := d.pfAdmitVote(2, candidate, ubo); err != nil {
		t.Fatalf("expiring vote: %v", err)
	}
	time.Sleep(30 * time.Second)
	if seated(t, d, ctx, candidate) {
		t.Fatalf("PRECONDITION FAILED: %s admitted before the second round", candidate)
	}

	// Second round: 4 of 5 seats (the bar) vote back to back, well inside a window.
	for v := 1; v <= 4; v++ {
		if _, err := d.pfAdmitVote(v, candidate, ubo); err != nil {
			t.Fatalf("round-2 vote from magi-%d: %v", v, err)
		}
	}
	time.Sleep(60 * time.Second)
	if !seated(t, d, ctx, candidate) {
		t.Errorf("POA-7 REPRODUCED (floor %d): 4 of 5 seats voted for %s after its first proposal expired and it was NOT admitted; the pair is barred forever",
			floor, candidate)
		return
	}
	t.Logf("POA-7 FIXED (floor %d): the expired pair re-opened and %s was admitted on the second round", floor, candidate)
}

// POA-5: a delegator must not pull the bond of a node that is under the POA
// collateral lock. Old rules test the SIGNER, so an unseated delegator's unstake
// from an active seat goes through.
func TestPoaFixDelegatorCannotUnlockASeatsBond(t *testing.T) {
	d, ctx, floor := poaFixDevnet(t, 0, true, 60*time.Minute)
	const A, B = 1, 2
	delegator, node := d.witnessAccount(A), d.witnessAccount(B)
	delegatorFull, nodeFull := "hive:"+delegator, "hive:"+node

	// Make the delegator an unseated account (it must not be caught by the
	// signer-side lock, which is what the finding is about). Every node drops
	// the same row, so the registry stays consistent.
	base, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("election: %v", err)
	}
	keep := slices.DeleteFunc(bareAccounts(base.Members), func(a string) bool { return a == delegator })
	trimRegistryEverywhere(t, d, ctx, keep, 5)
	if seated(t, d, ctx, delegator) {
		t.Fatalf("PRECONDITION FAILED: %s still holds a seat", delegator)
	}
	if !seated(t, d, ctx, node) {
		t.Fatalf("PRECONDITION FAILED: %s holds no seat", node)
	}

	if _, err := d.Deposit(ctx, A, "100.000", "hive"); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if !waitBalancePositive(t, d, ctx, 1, delegatorFull, "hive", 3*time.Minute) {
		t.Fatal("deposit never credited")
	}
	if _, err := d.ConsensusStake(A, B, "5.000"); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if del, _ := d.consensusDelegation(ctx, 1, delegatorFull, nodeFull); del == 5000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PRECONDITION FAILED: delegation edge never reached 5000")
		}
		time.Sleep(5 * time.Second)
	}

	// The node is an active, electable seat: its collateral is locked.
	// Retried on a transport error: repeating is safe here, because a second
	// 5.000 unstake is either refused like the first or fails on balance.
	var uerr error
	for i := 0; i < 4; i++ {
		if _, uerr = d.ledgerOp("vsc.consensus_unstake", delegator, node, "5.000", "hive", ""); uerr == nil {
			break
		}
		t.Logf("unstake broadcast attempt %d: %v", i+1, uerr)
		time.Sleep(10 * time.Second)
	}
	if uerr != nil {
		t.Fatalf("unstake broadcast: %v", uerr)
	}
	time.Sleep(60 * time.Second)
	del, err := d.consensusDelegation(ctx, 1, delegatorFull, nodeFull)
	if err != nil {
		t.Fatalf("reading the edge: %v", err)
	}
	if del != 5000 {
		t.Errorf("POA-5 REPRODUCED (floor %d): delegator %s unstaked from active seat %s (edge 5000 -> %d); the lock tested only the signer",
			floor, delegator, node, del)
		return
	}
	t.Logf("POA-5 FIXED (floor %d): unstake from a locked seat refused, edge still 5000", floor)
}
