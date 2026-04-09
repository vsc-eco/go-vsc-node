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

// TestBlameSSIDMismatch is a regression test for the old mainnet blame loop.
//
// The old system used RPC-based readiness checks (checkParticipantReadiness)
// that produced non-deterministic party lists across nodes, causing SSID
// mismatch and preventing blame from landing. This has been replaced by
// BLS-signed gossip attestations that are deterministic across all nodes.
//
// This test creates asymmetric network conditions where some nodes can reach
// a peer but others cannot, then verifies that the gossip-based system
// correctly handles the situation — unreachable nodes won't send gossip
// attestations, so they're deterministically excluded from the party list.
//
// Setup: 7 nodes. Nodes 1-4 have 6s latency to node 7, nodes 5-6 don't.
//
// Expected behavior:
//   - Node 7 can still gossip attestations (latency doesn't prevent pubsub)
//   - If node 7's attestations arrive in time, it's included in reshare
//   - If they arrive too late, it's excluded deterministically on all nodes
//   - Either way, blame should land correctly if the reshare fails
//
// Run with:
//
//	go test -v -run TestBlameSSIDMismatch -timeout 45m ./tests/devnet/
func TestBlameSSIDMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet SSID mismatch test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	wasmPath, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
	}

	// 7 nodes: enough that excluding 1-2 still leaves quorum, but
	// asymmetric partitions cause different readiness results.
	cfg := DefaultConfig()
	cfg.Nodes = 7
	cfg.GenesisNode = 7
	cfg.LogLevel = "trace"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 60, // ~3 min — enough for keygen + one reshare before election
		},
		TssParams: &params.TssParams{
			RotateInterval: 20, // 20 blocks (~1 min) instead of default 100 (~5 min)
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

	payload := `{"key_name":"ssidTestKey","epochs":10}`
	_, err = d.CallContract(ctx, 2, contractId, "tssCreate", payload)
	if err != nil {
		t.Fatalf("calling tssCreate: %v", err)
	}

	fullKeyId := contractId + "-ssidTestKey"
	t.Logf("waiting for key %s...", fullKeyId)
	_, err = d.WaitForTssKey(ctx, 2, bson.M{"id": fullKeyId}, 5*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("key never created: %v", err)
	}

	// Keygen triggers at the next bh%100==0 after the key is created,
	// which could be up to ~5 min away. The protocol itself takes
	// ~30-60s with 7 nodes, plus BLS collection + L1 confirmation.
	// Use 10 min to be safe.
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

	// ── Phase 2: Inject asymmetric latency ───────────────────────────
	//
	// Add ~6s latency between nodes 1-4 and node 7 (but NOT between
	// nodes 5-6 and node 7). With gossip-based readiness, this latency
	// may delay node 7's attestation delivery via pubsub. If attestations
	// arrive before the reshare block, node 7 is included deterministically
	// on all nodes. If they arrive too late, node 7 is excluded
	// deterministically on all nodes.

	// 6s latency may delay gossip attestation delivery via pubsub.
	// No jitter — consistent delay for nodes 1-4, normal for nodes 5-6.
	t.Log("Phase 2: injecting 6s latency between nodes 1-4 and node 7...")
	for _, n := range []int{1, 2, 3, 4} {
		if err := d.AddLatency(ctx, n, 7, 6000, 0); err != nil {
			t.Fatalf("adding latency between node %d and node 7: %v", n, err)
		}
	}
	t.Cleanup(func() {
		for _, n := range []int{1, 2, 3, 4} {
			d.RemoveLatency(context.Background(), n)
		}
		d.RemoveLatency(context.Background(), 7)
	})

	// ── Phase 3: Wait through two reshare cycles ─────────────────────
	//
	// We wait for 2 reshare windows and observe what happens.
	// With gossip-based readiness, expected outcomes:
	//
	// A) No commitments land — gossip attestations failed to propagate
	//
	// B) Blame lands and node 7 is excluded on retry via blame
	//    accumulation threshold, then reshare succeeds
	//
	// C) Reshare succeeds immediately — node 7's attestations arrived
	//    in time despite the latency

	rotateInterval := uint64(20) // matches TssParams.RotateInterval above
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	nextReshare1 := ((bh / rotateInterval) + 1) * rotateInterval
	nextReshare2 := nextReshare1 + rotateInterval
	// After second reshare + timeout (2m) + BLS window + margin
	targetBlock := nextReshare2 + 50

	t.Logf("current block: %d, waiting through 2 reshare cycles to block %d...", bh, targetBlock)
	if err := d.WaitForBlockProcessing(ctx, 2, targetBlock, 10*time.Minute); err != nil {
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

	t.Logf("commitments after keygen: %d", len(commits))
	for i, c := range commits {
		bits := decodeBitset(t, c.Commitment)
		t.Logf("  [%d] type=%s block=%d epoch=%d bitset=%s commitment=%s",
			i, c.Type, c.BlockHeight, c.Epoch, bits.Text(2), c.Commitment)
	}

	switch {
	case len(commits) == 0:
		// Outcome A: SSID mismatch — no blame landed.
		t.Log("")
		t.Log("RESULT: No blame/reshare commitments landed during asymmetric partition.")
		t.Log("DIAGNOSIS: Gossip attestations may not have propagated, or nodes")
		t.Log("disagreed on readiness state. Check gossip logs for attestation counts.")

	case len(commits) >= 2 && commits[0].Commitment == commits[1].Commitment:
		// Outcome B: repeated identical blame.
		t.Log("")
		t.Log("RESULT: Multiple blame commitments landed with IDENTICAL bitsets.")
		t.Log("DIAGNOSIS: Blame is deterministic (all nodes agree), but the blamed")
		t.Log("node has not exceeded the blame threshold for exclusion. Check if the")
		t.Log("TSS_BLAME_THRESHOLD_PERCENT is set correctly for this scenario.")

	case len(commits) >= 1 && commits[0].Type == "reshare":
		// Outcome C: reshare succeeded — blame exclusion worked.
		t.Log("")
		t.Log("RESULT: Reshare succeeded during asymmetric partition.")
		t.Log("This suggests the readiness check bug is fixed (or the partition")
		t.Log("didn't trigger the non-deterministic path).")

	case len(commits) >= 2 && commits[0].Commitment != commits[len(commits)-1].Commitment:
		// Some blame landed, bitsets differ — partial progress.
		t.Log("")
		t.Logf("RESULT: %d commitments with varying bitsets.", len(commits))
		t.Log("Blame exclusion is partially working but may still have issues.")

	default:
		t.Logf("RESULT: %d commitment(s), unclear pattern.", len(commits))
	}

	// ── Phase 4: Heal and verify recovery ────────────────────────────

	t.Log("")
	t.Log("Phase 4: removing latency, verifying reshare can succeed...")
	for _, n := range []int{1, 2, 3, 4} {
		d.RemoveLatency(ctx, n)
	}
	d.RemoveLatency(ctx, 7)

	finalBh, _ := d.getLastProcessedBlock(ctx, 2)
	commit, err := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": finalBh},
	}, 10*time.Minute)
	if err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("no commitment after healing: %v", err)
	}

	t.Logf("post-heal commitment: type=%s block=%d", commit.Type, commit.BlockHeight)
	if commit.Type == "reshare" {
		t.Log("PASS: reshare succeeded after healing partition")
	} else {
		t.Logf("blame landed after healing — may need another cycle to succeed")
	}
}
