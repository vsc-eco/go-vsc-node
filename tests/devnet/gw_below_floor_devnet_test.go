package devnet

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestGatewayCommitteeBelowFloor: "if it drops to 7 what happens, can it go
// back up?" (owner 10-31), with mainnet's floors: MinMembers 8 (an election with
// fewer is rejected from 0.4: state-processing/system_txs.go:749) and the
// gateway's 8-key rotation floor (gateway/quorum.go:20). The gateway follows the
// ELECTION (keyRotation reads the election in force), not TSS; a new election is
// signed by 2/3 of the OUTGOING committee, a gateway rotation by 2/3 of the
// keys it replaces.
//
// 10 nodes, floor 0.12 (seat gate + top-up), MinMembers 8:
//
//	P0  baseline: epoch advancing, withdrawal paid.
//	P1  magi-8/9/10 disable their witness (nodes keep running): 7 candidates.
//	    Expect: elections stop (the last 10-member committee stays in force),
//	    the gateway keeps its 10 keys, withdrawals still paid (all key holders
//	    online).
//	P2  magi-10 re-enables: 8 candidates. Expect: elections resume with 8
//	    members, the gateway rotates to their 8 keys, a withdrawal is paid.
//
//	go test -v -run TestGatewayCommitteeBelowFloor -timeout 150m ./tests/devnet/
func TestGatewayCommitteeBelowFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := tssTestConfig()
	cfg.Nodes = 10
	cfg.SkipFunding = false
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorConsensus, cp.ConsensusVersionFloorEpoch = 0, 12, 1
	cp.MinMembers = 8 // mainnet
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_GATEWAY_ROTATION_INTERVAL"] = "20"
	cfg.MagiEnv["VSC_GATEWAY_ACTION_INTERVAL"] = "20"
	d, ctx := startDevnetNoKey(t, cfg, 120*time.Minute)

	if err := d.waitForElectionEpoch(ctx, 1, 2, 20*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: no epoch 2: %v", err)
	}
	keyCount := func() int { _, w := gwActiveWeights(t, d); return len(w) }
	for end := devnetNow().Add(20 * time.Minute); keyCount() < 8; devnetSleep(15 * time.Second) {
		if devnetNow().After(end) {
			t.Fatalf("PRECONDITION FAILED: no rotation installed the committee keys (authority %s)", gwAuthority(t, d))
		}
	}
	t.Logf("RESULT GW-F0: line 0.%d, authority %s", vfActiveConsensus(d, ctx), gwAuthority(t, d))

	payer, payeeAcct := 1, d.witnessAccount(2)
	if _, err := d.Deposit(ctx, payer, "10.000", "hbd"); err != nil {
		t.Fatalf("PRECONDITION FAILED: HBD deposit: %v", err)
	}
	if !waitBalancePositive(t, d, ctx, 1, "hive:"+d.witnessAccount(payer), "hbd", 5*time.Minute) {
		t.Fatalf("PRECONDITION FAILED: HBD deposit never credited")
	}
	withdraw := func(label string) bool {
		before := gwL1Hbd(t, d, payeeAcct)
		tx, err := d.Withdraw(payer, payeeAcct, "1.000", "hbd", label)
		if err != nil {
			t.Fatalf("%s: withdraw: %v", label, err)
		}
		start := devnetNow()
		for end := devnetNow().Add(15 * time.Minute); devnetNow().Before(end); devnetSleep(10 * time.Second) {
			if gwL1Hbd(t, d, payeeAcct) >= before+1000 {
				t.Logf("RESULT %s: paid after %s (tx %s)", label, devnetSince(start).Round(time.Second), tx)
				return true
			}
		}
		t.Logf("RESULT %s: NOT paid within 15 min (tx %s)", label, tx)
		return false
	}
	if !withdraw("GW-F0 baseline") {
		t.Fatalf("baseline withdrawal not paid")
	}

	// P1: three leave, nodes keep running: 7 candidates.
	e0 := custodyCurrentEpoch(d, ctx, 1)
	for _, n := range []int{8, 9, 10} {
		disableWitnessEverywhere(t, d, ctx, d.witnessAccount(n), cfg.Nodes)
	}
	devnetSleep(8 * time.Minute) // several election intervals (20 blocks each)
	e1 := custodyCurrentEpoch(d, ctx, 1)
	m1, _ := d.GetElectionMembers(ctx, 1, e1)
	t.Logf("RESULT GW-F1: 7 candidates: epoch %d -> %d, members of the election in force %d, authority %s", e0, e1, len(m1), gwAuthority(t, d))
	if len(m1) < 8 {
		t.Errorf("GW-F1: an election with %d members is in force; mainnet's floor is 8", len(m1))
	}
	withdraw("GW-F1 withdrawal with 7 candidates")

	// P2: one leaver comes back: 8 candidates.
	setWitnessEnabledEverywhere(t, d, ctx, d.witnessAccount(10), cfg.Nodes, true)
	resumed := false
	for end := devnetNow().Add(20 * time.Minute); devnetNow().Before(end) && !resumed; devnetSleep(15 * time.Second) {
		e := custodyCurrentEpoch(d, ctx, 1)
		m, _ := d.GetElectionMembers(ctx, 1, e)
		resumed = e > e1 && len(m) == 8
	}
	e2 := custodyCurrentEpoch(d, ctx, 1)
	m2, _ := d.GetElectionMembers(ctx, 1, e2)
	rotated := false
	for end := devnetNow().Add(15 * time.Minute); devnetNow().Before(end) && !rotated; devnetSleep(15 * time.Second) {
		rotated = keyCount() == 8
	}
	t.Logf("RESULT GW-F2: back to 8 candidates: elections resumed=%v (epoch %d, %d members), gateway rotated to 8 keys=%v, authority %s", resumed, e2, len(m2), rotated, gwAuthority(t, d))
	paid := withdraw("GW-F2 withdrawal after recovery")
	t.Logf("SUMMARY: at 7 candidates elections stopped=%v and the old committee kept the gateway; back at 8: resumed=%v, rotated=%v, paid=%v", e1 == e0 || len(m1) >= 8, resumed, rotated, paid)
	if !resumed || !rotated {
		t.Errorf("the committee and gateway did not recover when an 8th candidate returned")
	}
}

// setWitnessEnabledEverywhere flips a witness's enabled flag in every node's
// witnesses collection (the same deterministic edit disableWitnessEverywhere
// makes).
func setWitnessEnabledEverywhere(t *testing.T, d *Devnet, ctx context.Context, account string, nodes int, enabled bool) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	for n := 1; n <= nodes; n++ {
		if _, err := client.Database(d.nodeDbName(n)).Collection("witnesses").UpdateMany(ctx,
			bson.M{"account": account}, bson.M{"$set": bson.M{"enabled": enabled}}); err != nil {
			t.Fatalf("setting enabled=%v for %s on magi-%d: %v", enabled, account, n, err)
		}
	}
}
