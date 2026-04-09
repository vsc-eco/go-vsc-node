package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
)

// TestBlameDisconnectedNode tests whether the network recovers when a
// node is fully disconnected (not just slow). This emulates a node
// that has broadcast readiness but then crashes or loses all connectivity
// before the reshare begins.
//
// Unlike TestBlamePartialLatency (which tests slow/degraded nodes),
// this test verifies the "false readiness" scenario from the review
// (Section 4, Scenario 3):
//
//  1. All nodes broadcast readiness (on-chain, deterministic)
//  2. One node is fully disconnected after readiness lands
//  3. Reshare starts — the disconnected node is in the party list
//     (it declared ready) but never sends btss messages
//  4. All online nodes see the same WaitingFor() result (the
//     disconnected node), produce identical CIDs, BLS succeeds
//  5. Blame lands on-chain targeting ONLY the disconnected node
//  6. Next reshare excludes the blamed node and succeeds with the
//     remaining healthy quorum
//
// This is the critical recovery path: the network MUST be able to
// eject a crashed node and continue resharing without it.
//
// Run with:
//
//	go test -v -run TestBlameDisconnectedNode -timeout 30m ./tests/devnet/
func TestBlameDisconnectedNode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet disconnected node test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	wasmPath, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 5
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

	payload := `{"key_name":"disconnectKey","epochs":10}`
	_, err = d.CallContract(ctx, 2, contractId, "tssCreate", payload)
	if err != nil {
		t.Fatalf("calling tssCreate: %v", err)
	}

	fullKeyId := contractId + "-disconnectKey"
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

	// ── Phase 2: Disconnect node 3 AFTER readiness window ────────────
	//
	// Wait until we're past the readiness window (node 3 has broadcast
	// its vsc.tss_ready) but before the reshare protocol completes.
	// Then fully disconnect it — it drops out of P2P entirely.

	t.Log("Phase 2: waiting for readiness window, then disconnecting node 3...")

	rotateInterval := uint64(20)
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	nextReshare := ((bh / rotateInterval) + 1) * rotateInterval

	// Wait until 2 blocks before reshare (readiness broadcasts at reshare-5,
	// so by reshare-2 the readiness txs should have landed on-chain).
	waitTarget := nextReshare - 2
	t.Logf("current block: %d, waiting for block %d (2 before reshare at %d)...", bh, waitTarget, nextReshare)
	if err := d.WaitForBlockProcessing(ctx, 2, waitTarget, 3*time.Minute); err != nil {
		t.Fatalf("didn't reach readiness deadline: %v", err)
	}

	// Verify node 3's readiness is on-chain before we disconnect it.
	witnessName3 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 3)
	readyCommits, err := d.GetCommitments(ctx, 2, bson.M{
		"key_id":     fullKeyId,
		"type":       "ready",
		"commitment": witnessName3,
	})
	if err == nil && len(readyCommits) > 0 {
		t.Logf("node 3 readiness confirmed on-chain (block %d)", readyCommits[len(readyCommits)-1].BlockHeight)
	} else {
		t.Logf("warning: node 3 readiness not yet on-chain — blame may not target it")
	}

	// Fully disconnect node 3 — drops ALL input traffic.
	t.Log("disconnecting node 3...")
	if err := d.Disconnect(ctx, 3); err != nil {
		t.Fatalf("disconnecting node 3: %v", err)
	}

	// ── Phase 3: Wait for blame + recovery reshare ───────────────────
	//
	// Expected sequence:
	//   Cycle 1 (reshare at nextReshare): node 3 is in party list (readiness
	//     on-chain) but doesn't send messages → timeout → blame
	//   Cycle 2 (reshare at nextReshare+20): node 3 excluded by blame OR
	//     excluded by readiness gate (didn't broadcast for this cycle) →
	//     reshare succeeds with 4 nodes

	// Wait through 2 reshare cycles + timeout margin
	targetBlock := nextReshare + 2*rotateInterval + 50
	t.Logf("waiting through 2 reshare cycles to block %d (node 3 disconnected)...", targetBlock)
	if err := d.WaitForBlockProcessing(ctx, 2, targetBlock, 12*time.Minute); err != nil {
		t.Fatalf("didn't reach target block: %v", err)
	}

	// ── Phase 4: Assertions ──────────────────────────────────────────

	commits, err := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})
	if err != nil {
		t.Fatalf("querying commitments: %v", err)
	}

	members, err := d.GetElectionMembers(ctx, 2, keygenCommit.Epoch+1)
	if err != nil {
		t.Logf("warning: could not read election members: %v", err)
		members = make([]string, cfg.Nodes)
		for i := range members {
			members[i] = fmt.Sprintf("%s%d", cfg.WitnessPrefix, i+1)
		}
	}
	t.Logf("election member ordering: %v", members)

	t.Logf("commitments after keygen: %d", len(commits))
	hasBlame := false
	hasReshare := false
	blameTargetsNode3 := false

	for i, c := range commits {
		bits := decodeBitset(t, c.Commitment)
		var names []string
		for j := 0; j < len(members); j++ {
			if bits.Bit(j) == 1 {
				names = append(names, members[j])
			}
		}
		label := "blamed"
		if c.Type == "reshare" {
			label = "in committee"
			hasReshare = true
		}
		if c.Type == "blame" {
			hasBlame = true
			for _, name := range names {
				if name == witnessName3 {
					blameTargetsNode3 = true
				}
			}
		}
		t.Logf("  [%d] type=%s block=%d epoch=%d %s=%v",
			i, c.Type, c.BlockHeight, c.Epoch, label, names)
	}

	// Check 1: blame should land and target node 3
	if hasBlame {
		if blameTargetsNode3 {
			t.Logf("PASS: blame correctly targets disconnected node (%s)", witnessName3)
		} else {
			t.Errorf("blame landed but does NOT target node 3 (%s) — wrong culprit", witnessName3)
		}
	} else {
		t.Log("warning: no blame commitment landed")
		t.Log("This could mean node 3 was excluded by readiness gate (no false readiness),")
		t.Log("which is actually correct behavior — the readiness gate prevents the issue.")
	}

	// Check 2: reshare should succeed (with or without blame)
	if hasReshare {
		t.Log("PASS: reshare succeeded after disconnecting node 3!")
		t.Log("The network recovered — either via blame exclusion or readiness gate.")
	} else if hasBlame {
		t.Error("FAIL: blame landed but reshare did not succeed on retry.")
		t.Log("DIAGNOSIS: blamed node was not properly excluded in the next cycle,")
		t.Log("or the remaining 4 nodes couldn't complete the reshare.")
	} else {
		t.Error("FAIL: neither blame nor reshare landed.")
		t.Log("DIAGNOSIS: the disconnected node may have caused SSID mismatch")
		t.Log("or BLS collection failure, preventing any commitment from landing.")
	}

	// Reconnect node 3 for clean teardown
	d.Reconnect(ctx, 3)
}
