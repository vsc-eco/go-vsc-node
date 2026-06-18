package devnet

import (
	"context"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// consensusDelegation reads getConsensusDelegation(from,to).delegated from a node.
// from/to are full L2 accounts ("hive:name").
func (d *Devnet) consensusDelegation(ctx context.Context, node int, from, to string) (int64, error) {
	const q = `query($f:String!,$t:String!){getConsensusDelegation(from:$f,to:$t){delegated claimable}}`
	var out struct {
		GetConsensusDelegation *struct {
			Delegated int64 `json:"delegated"`
			Claimable int64 `json:"claimable"`
		} `json:"getConsensusDelegation"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"f": from, "t": to}, &out); err != nil {
		return 0, err
	}
	if out.GetConsensusDelegation == nil {
		return 0, nil
	}
	return out.GetConsensusDelegation.Delegated, nil
}

// TestConsensusDelegatedStakeDevnet runs the per-delegator stake/unstake feature
// on a real multi-node devnet with consensus 0.2.0 pinned active. userA (witness
// 1) delegates consensus stake to operatorB (witness 2); the per-edge delegation
// must appear on every node, and userA's undelegate must drain it — proving the
// gated 0.2.0 path works end-to-end and deterministically across nodes.
func TestConsensusDelegatedStakeDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.SkipFunding = false // need L2 HIVE to consensus-stake
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// Pin consensus 0.2.0 from epoch 1 so the delegation path is live. NOTE:
	// FloorEpoch must be NON-ZERO — PinnedVersionFloor treats 0 as "no floor".
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 2
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 20*time.Minute)

	// Wait for the first running election (epoch >= 1) on every node.
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 1, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
	}

	const A, B = 1, 2
	userA := d.witnessAccount(A)     // bare "magi.test1" — for ops/required_auths
	operatorB := d.witnessAccount(B) // bare "magi.test2"
	userAFull := "hive:" + userA     // L2 account / edge component
	operatorBFull := "hive:" + operatorB

	// Fund userA and WAIT for the deposit to credit the VSC ledger before staking.
	if _, err := d.Deposit(ctx, A, "100.000", "hive"); err != nil {
		t.Fatalf("deposit for userA: %v", err)
	}
	if !waitBalancePositive(t, d, ctx, 1, userAFull, "hive", 3*time.Minute) {
		t.Fatal("deposit never credited userA's VSC hive balance")
	}

	// userA delegates 5.000 HIVE of consensus stake to operatorB (from != to).
	t.Logf("userA=%s delegates 5.000 consensus stake to operatorB=%s", userAFull, operatorBFull)
	if _, err := d.ConsensusStake(A, B, "5.000"); err != nil {
		t.Fatalf("delegated stake A->B: %v", err)
	}

	// Poll until the per-edge delegation appears (stake processed AND 0.2.0 active).
	var edgeOK bool
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if del, _ := d.consensusDelegation(ctx, 1, userAFull, operatorBFull); del == 5000 {
			edgeOK = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !edgeOK {
		hc := int64(-1)
		if bal, err := d.GetAccountBalance(ctx, 1, operatorBFull); err == nil && bal != nil {
			hc = bal.HiveConsensus
		}
		t.Fatalf("edge never reached 5000; operatorB hive_consensus=%d (>0 => 0.2.0 inactive; 0 => stake did not process)", hc)
	}

	// Deterministic across the whole network: the edge is 5000 on EVERY node.
	for n := 1; n <= cfg.Nodes; n++ {
		del, err := d.consensusDelegation(ctx, n, userAFull, operatorBFull)
		if err != nil {
			t.Fatalf("magi-%d getConsensusDelegation: %v", n, err)
		}
		t.Logf("magi-%d: delegated(A->B) = %d (expect 5000)", n, del)
		if del != 5000 {
			t.Errorf("magi-%d: expected delegated=5000, got %d", n, del)
		}
	}

	// userA undelegates the full amount — the edge drains immediately (the HIVE
	// itself returns after the unbond epochs; the edge balance is the assertion).
	if _, err := d.ledgerOp("vsc.consensus_unstake", userA, operatorB, "5.000", "hive", ""); err != nil {
		t.Fatalf("delegated unstake A->B: %v", err)
	}
	// Poll until the edge drains to 0.
	var drained bool
	deadline = time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if del, _ := d.consensusDelegation(ctx, 1, userAFull, operatorBFull); del == 0 {
			drained = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !drained {
		t.Fatal("edge never drained to 0 after undelegate")
	}
	for n := 1; n <= cfg.Nodes; n++ {
		del, err := d.consensusDelegation(ctx, n, userAFull, operatorBFull)
		if err != nil {
			t.Fatalf("magi-%d getConsensusDelegation post-unstake: %v", n, err)
		}
		t.Logf("magi-%d: delegated(A->B) after undelegate = %d (expect 0)", n, del)
		if del != 0 {
			t.Errorf("magi-%d: expected edge drained to 0, got %d", n, del)
		}
	}
}
