package devnet

import (
	"context"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
)

// TestBlameBidirectionalLatency tests whether the network can make
// progress when 2 of 7 nodes have BIDIRECTIONAL latency to some peers.
//
// This is harder than unidirectional latency because the slow nodes'
// responses to healthy nodes are also delayed, which can cause healthy
// nodes to be blamed too. Under real network conditions, latency is
// typically bidirectional (a slow path is slow in both directions).
//
// Current behavior: 4+ nodes get blamed (the 2 slow nodes + healthy
// nodes that appear slow FROM the slow nodes' perspective), leaving
// insufficient quorum for reshare. The network is stuck.
//
// Future fix needed: the protocol should handle bidirectional latency
// gracefully, perhaps by weighting blame toward nodes that multiple
// peers agree are slow, or by using on-chain readiness signals that
// are independent of per-peer latency.
//
// Latency setup (bidirectional, 6s):
//
//	Node 1 ↔ Node 6: 6s     Node 1 ↔ Node 7: 6s
//	Node 2 ↔ Node 6: 6s     Node 2 ↔ Node 7: normal
//	Node 3 ↔ Node 6: normal Node 3 ↔ Node 7: 6s
//	Node 4 ↔ Node 6: normal Node 4 ↔ Node 7: normal
//	Node 5 ↔ Node 6: normal Node 5 ↔ Node 7: normal
//
// Run with:
//
//	go test -v -run TestBlameBidirectionalLatency -timeout 30m ./tests/devnet/
func TestBlameBidirectionalLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet partial latency test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	wasmPath, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Nodes = 7
	cfg.GenesisNode = 7
	cfg.LogLevel = "trace"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 60,
		},
		TssParams: &params.TssParams{
			RotateInterval: 20,
		},
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}

	// ── Phase 1: Deploy contract, keygen, wait for epoch 1 ───────────

	t.Log("Phase 1: deploying contract and triggering keygen...")

	contractId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     wasmPath,
		Name:         "call-tss",
		DeployerNode: 1,
		GQLNode:      2,
	})
	if err != nil {
		t.Fatalf("deploying contract: %v", err)
	}

	if err := d.WaitForBlockProcessing(ctx, 2, 10, 3*time.Minute); err != nil {
		t.Fatalf("node 2 never synced: %v", err)
	}

	payload := `{"key_name":"bidirKey","epochs":10}`
	_, err = d.CallContract(ctx, 2, contractId, "tssCreate", payload)
	if err != nil {
		t.Fatalf("calling tssCreate: %v", err)
	}

	fullKeyId := contractId + "-bidirKey"
	t.Logf("waiting for key %s...", fullKeyId)
	_, err = d.WaitForTssKey(ctx, 2, bson.M{"id": fullKeyId}, 5*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("key never created: %v", err)
	}

	keygenCommit, err := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id": fullKeyId,
		"type":   "keygen",
	}, 10*time.Minute)
	if err != nil {
		d.dumpBlockHeight(ctx, t, 2)
		d.dumpTssLogs(ctx, t, 2)
		t.Fatalf("keygen commitment never landed: %v", err)
	}
	t.Logf("keygen at block %d epoch %d", keygenCommit.BlockHeight, keygenCommit.Epoch)

	_, err = d.WaitForTssKey(ctx, 2, bson.M{
		"id": fullKeyId, "status": "active",
	}, 2*time.Minute)
	if err != nil {
		t.Fatalf("key never became active: %v", err)
	}

	t.Log("waiting for epoch >= 1...")
	if err := d.waitForElectionEpoch(ctx, 2, 1, 5*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("epoch 1 never arrived: %v", err)
	}

	// ── Phase 2: Inject asymmetric latency to 2 "slow" nodes ─────────
	//
	// Nodes 6 and 7 become slow. Different healthy nodes see different
	// combinations of them as reachable vs timed-out.

	t.Log("Phase 2: injecting BIDIRECTIONAL latency to nodes 6 and 7...")

	// Bidirectional: both directions are delayed. This means nodes
	// 1-3 appear slow FROM nodes 6/7's perspective too, causing
	// more nodes to be blamed than intended.

	// Node 6 ↔ nodes 1,2: slow both ways
	for _, n := range []int{1, 2} {
		if err := d.AddLatency(ctx, n, 6, 6000, 0); err != nil {
			t.Fatalf("adding latency %d↔6: %v", n, err)
		}
	}

	// Node 7 ↔ nodes 1,3: slow both ways
	for _, n := range []int{1, 3} {
		if err := d.AddLatency(ctx, n, 7, 6000, 0); err != nil {
			t.Fatalf("adding latency %d↔7: %v", n, err)
		}
	}

	t.Cleanup(func() {
		for _, n := range []int{1, 2, 3, 6, 7} {
			d.RemoveLatency(context.Background(), n)
		}
	})

	t.Log("  Bidirectional latency (6s):")
	t.Log("  Node 1 ↔ Node 6: slow    Node 1 ↔ Node 7: slow")
	t.Log("  Node 2 ↔ Node 6: slow    Node 2 ↔ Node 7: normal")
	t.Log("  Node 3 ↔ Node 6: normal  Node 3 ↔ Node 7: slow")
	t.Log("  Node 4 ↔ Node 6: normal  Node 4 ↔ Node 7: normal")
	t.Log("  Node 5 ↔ Node 6: normal  Node 5 ↔ Node 7: normal")

	// ── Phase 3: Wait for the network to self-heal under latency ─────
	//
	// The latency stays active throughout this phase. We wait for up
	// to 4 reshare cycles. The correct behavior is:
	//
	//   Cycle 1: blame lands (5 healthy nodes agree on culprits)
	//   Cycle 2: reshare succeeds with 5 nodes (blamed nodes excluded)
	//
	// With the current bug, neither blame nor reshare will land because
	// nodes disagree on the party list.

	rotateInterval := uint64(20)
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	// Wait through 4 reshare cycles to give the blame→exclude→reshare
	// pipeline time to complete while latency is still active.
	targetBlock := ((bh/rotateInterval)+1)*rotateInterval + 4*rotateInterval + 50

	t.Logf("current block: %d, waiting through 4 reshare cycles (latency active) to block %d...", bh, targetBlock)
	if err := d.WaitForBlockProcessing(ctx, 2, targetBlock, 12*time.Minute); err != nil {
		t.Fatalf("didn't reach target block: %v", err)
	}

	// Collect all blame/reshare commitments since keygen.
	commits, err := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})
	if err != nil {
		t.Fatalf("querying commitments: %v", err)
	}

	t.Logf("commitments during obstructed network: %d", len(commits))
	for i, c := range commits {
		bits := decodeBitset(t, c.Commitment)
		t.Logf("  [%d] type=%s block=%d epoch=%d bitset=%s commitment=%s",
			i, c.Type, c.BlockHeight, c.Epoch, bits.Text(2), c.Commitment)
	}

	// Check if a reshare succeeded while latency was still active.
	hasReshare := false
	for _, c := range commits {
		if c.Type == "reshare" {
			hasReshare = true
			break
		}
	}

	switch {
	case hasReshare:
		t.Log("")
		t.Log("RESULT: Reshare succeeded under obstructed network!")
		t.Log("The 5 healthy nodes agreed on a blame, excluded the slow")
		t.Log("nodes, and completed a reshare among themselves.")
		t.Log("This is the CORRECT behavior.")

	case len(commits) == 0:
		t.Log("")
		t.Log("RESULT: No commitments landed under obstructed network.")
		t.Log("DIAGNOSIS: Even with 5/7 healthy nodes (above 2/3 threshold),")
		t.Log("the non-deterministic readiness check prevents BLS consensus")
		t.Log("on any blame CID. The network cannot make progress.")
		t.Log("")
		t.Log("EXPECTED WHEN FIXED: Blame should land (5 healthy nodes agree)")
		t.Log("followed by a successful reshare excluding the slow nodes,")
		t.Log("all while the slow nodes remain slow.")
		t.Fail()

	case len(commits) >= 2 && commits[0].Commitment == commits[len(commits)-1].Commitment:
		t.Log("")
		t.Log("RESULT: Repeated identical blame — same nodes blamed each cycle.")
		t.Log("Blame landed but exclusion isn't working. The network is stuck")
		t.Log("in a blame loop even though 5 healthy nodes have quorum.")
		t.Fail()

	case len(commits) >= 1:
		t.Log("")
		t.Logf("RESULT: %d commitment(s) landed but no reshare succeeded.", len(commits))
		t.Log("Partial progress — blame is landing but the network hasn't")
		t.Log("completed a reshare among the healthy nodes yet.")
		t.Fail()

	default:
		t.Logf("RESULT: %d commitment(s), pattern unclear.", len(commits))
		t.Fail()
	}
}
