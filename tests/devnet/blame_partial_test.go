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

// TestBlamePartialLatency tests whether the network can make useful
// progress when 2 of 7 nodes are slow enough to fail readiness checks,
// but 5 healthy nodes (> 2/3 threshold) remain.
//
// The challenge: each healthy node sees a different subset of the 2
// slow nodes as reachable vs timed-out, depending on which latency
// paths happen to be configured. This means:
//
//   - Node 1 might see {6: timeout, 7: timeout}
//   - Node 2 might see {6: timeout, 7: ok}
//   - Node 3 might see {6: ok, 7: timeout}
//   - Node 4 might see {6: ok, 7: ok}
//   - Node 5 might see {6: ok, 7: ok}
//
// With the current buggy checkParticipantReadiness, each node builds a
// different party list → SSID mismatch → no blame lands.
//
// When fixed, nodes should build identical party lists from on-chain
// data only, let the protocol timeout handle slow nodes, and produce
// a blame commitment that the 5 healthy nodes agree on. The subsequent
// reshare should then exclude the blamed nodes and succeed with the
// remaining healthy quorum.
//
// Latency setup (deterministic, not random):
//
//	Nodes 6,7 are "slow". Latency is added asymmetrically:
//	  - Node 1 ↔ Node 6: 6s (timeout)     Node 1 ↔ Node 7: 6s (timeout)
//	  - Node 2 ↔ Node 6: 6s (timeout)     Node 2 ↔ Node 7: normal (ok)
//	  - Node 3 ↔ Node 6: normal (ok)      Node 3 ↔ Node 7: 6s (timeout)
//	  - Node 4 ↔ Node 6: normal (ok)      Node 4 ↔ Node 7: normal (ok)
//	  - Node 5 ↔ Node 6: normal (ok)      Node 5 ↔ Node 7: normal (ok)
//
// This guarantees different nodes see different combinations:
//   - Nodes 1,2 see node 6 as unreachable
//   - Nodes 1,3 see node 7 as unreachable
//   - Nodes 4,5 see both as reachable
//
// Run with:
//
//	go test -v -run TestBlamePartialLatency -timeout 30m ./tests/devnet/
func TestBlamePartialLatency(t *testing.T) {
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

	payload := `{"key_name":"partialKey","epochs":10}`
	_, err = d.CallContract(ctx, 2, contractId, "tssCreate", payload)
	if err != nil {
		t.Fatalf("calling tssCreate: %v", err)
	}

	fullKeyId := contractId + "-partialKey"
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

	t.Log("Phase 2: injecting asymmetric latency to nodes 6 and 7...")

	// Node 6 is slow from nodes 1,2 (but ok from 3,4,5)
	for _, n := range []int{1, 2} {
		if err := d.AddLatency(ctx, n, 6, 6000, 0); err != nil {
			t.Fatalf("adding latency %d↔6: %v", n, err)
		}
	}

	// Node 7 is slow from nodes 1,3 (but ok from 2,4,5)
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

	t.Log("  Node 1: sees 6=timeout, 7=timeout")
	t.Log("  Node 2: sees 6=timeout, 7=ok")
	t.Log("  Node 3: sees 6=ok,      7=timeout")
	t.Log("  Node 4: sees 6=ok,      7=ok")
	t.Log("  Node 5: sees 6=ok,      7=ok")

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

	case len(commits) >= 2 && commits[0].Commitment == commits[len(commits)-1].Commitment:
		t.Log("")
		t.Log("RESULT: Repeated identical blame — same nodes blamed each cycle.")
		t.Log("Blame landed but exclusion isn't working. The network is stuck")
		t.Log("in a blame loop even though 5 healthy nodes have quorum.")

	case len(commits) >= 1:
		t.Log("")
		t.Logf("RESULT: %d commitment(s) landed but no reshare succeeded.", len(commits))
		t.Log("Partial progress — blame is landing but the network hasn't")
		t.Log("completed a reshare among the healthy nodes yet.")

	default:
		t.Logf("RESULT: %d commitment(s), pattern unclear.", len(commits))
	}
}
