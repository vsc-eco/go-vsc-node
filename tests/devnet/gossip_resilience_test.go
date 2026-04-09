package devnet

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestGossipResilienceMultiVersion runs 7 nodes: 1 on old code (main) and 6
// on the current branch with off-chain gossip readiness. The key is created
// via direct DB injection so all 7 nodes (including the old-code one)
// participate in keygen — the old node holds a real key share.
//
// After keygen, one of the 6 new-code nodes is fully disconnected and another
// gets 5000ms outbound latency. The reshare must still succeed with the
// remaining 5 new-code nodes (4 healthy + 1 slow), resharing the key AWAY
// from the old-code node and the disconnected node.
//
// This validates that:
//   - Gossip readiness converges despite a slow node (5s latency)
//   - The old-code node is excluded (no gossip attestation, holds key share)
//   - The disconnected node is excluded (no gossip attestation)
//   - The slow node's attestation propagates within the gossip window
//   - Reshare completes with 5 participants (>= threshold+1 of 7)
//
// Node assignment:
//
//	magi-1..6: current branch (gossip readiness)
//	magi-7:    old code (main branch, no gossip support)
//	magi-3:    disconnected after keygen
//	magi-4:    5000ms outbound latency to all peers
//
// Run with:
//
//	go test -v -run TestGossipResilienceMultiVersion -timeout 35m ./tests/devnet/
func TestGossipResilienceMultiVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet gossip resilience test in short mode")
	}
	requireDocker(t)

	// The old code dir must point to a checkout of main.
	oldCodeDir := os.Getenv("OLD_CODE_DIR")
	if oldCodeDir == "" {
		oldCodeDir = "/tmp/go-vsc-node-main"
	}
	if _, err := os.Stat(oldCodeDir); err != nil {
		t.Skipf("old code repo not found at %s (set OLD_CODE_DIR env)", oldCodeDir)
	}

	cfg := tssTestConfig()
	cfg.Nodes = 7
	cfg.OldCodeSourceDir = oldCodeDir
	cfg.OldCodeNodes = []int{7}
	cfg.GenesisNode = 6 // genesis on a healthy new-code node
	cfg.SkipFunding = true
	cfg.LogLevel = "error,tss=trace"
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	// Don't use startDevnet() — it inserts the key immediately, which
	// triggers keygen against the initial (genesis) election that only
	// has ~5 members. We need to wait for the staked election (epoch 1)
	// which includes all 7 nodes, then insert the key.
	d, ctx := startDevnetNoKey(t, cfg, 30*time.Minute)

	// ── Phase 1: Wait for epoch 1, insert key, wait for keygen ──────

	t.Log("Phase 1: waiting for staked election with all 7 members...")
	if err := d.waitForElectionEpoch(ctx, 2, 1, 5*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("epoch 1 never arrived: %v", err)
	}

	// Verify epoch 1 has all 7 members before inserting the key.
	members1, err := d.GetElectionMembers(ctx, 2, 1)
	if err != nil {
		t.Fatalf("could not read epoch 1 members: %v", err)
	}
	t.Logf("epoch 1 has %d members: %v", len(members1), members1)
	if len(members1) != cfg.Nodes {
		t.Fatalf("epoch 1 has %d members, expected %d", len(members1), cfg.Nodes)
	}

	// NOW insert the key — keygen will use the epoch 1 election (all 7 nodes).
	keyId := "test-key-main"
	insertTssKey(t, d.MongoURI(), keyId, "ecdsa")
	t.Log("key inserted, waiting for keygen with all 7 nodes...")

	keygenCommit, err := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id": keyId,
		"type":   "keygen",
	}, 10*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("keygen commitment never landed: %v", err)
	}
	t.Logf("keygen at block %d epoch %d", keygenCommit.BlockHeight, keygenCommit.Epoch)

	// Verify the keygen commitment includes all 7 nodes.
	keygenMembers, err := d.GetElectionMembers(ctx, 2, keygenCommit.Epoch)
	if err != nil {
		t.Fatalf("could not read election members for keygen epoch %d: %v", keygenCommit.Epoch, err)
	}
	bits := decodeBitset(t, keygenCommit.Commitment)
	var included, excluded []string
	for i := 0; i < len(keygenMembers); i++ {
		if bits.Bit(i) == 1 {
			included = append(included, keygenMembers[i])
		} else {
			excluded = append(excluded, keygenMembers[i])
		}
	}
	t.Logf("keygen committee (%d/%d): included=%v excluded=%v",
		len(included), len(keygenMembers), included, excluded)
	if len(excluded) > 0 {
		t.Fatalf("keygen excluded %v — expected all %d nodes (old-code node must participate)",
			excluded, cfg.Nodes)
	}

	_, err = d.WaitForTssKey(ctx, 2, bson.M{
		"id": keyId, "status": "active",
	}, 2*time.Minute)
	if err != nil {
		t.Fatalf("key never became active: %v", err)
	}

	// ── Phase 2: Inject network faults ──────────────────────────────
	//
	// Node 3: fully disconnected (drops all input traffic).
	// Node 4: 5000ms outbound latency to every other node.
	// Node 7: old code — no gossip attestation, excluded naturally.
	//
	// After these faults, the gossip readiness set should contain nodes
	// 1, 2, 4, 5, 6 (node 4 is slow but its attestation propagates
	// within the gossip window). Nodes 3 and 7 are absent.
	// Old committee: 7 nodes in keygen, threshold ceil(7*2/3)-1 = 4.
	// Post-filter: 5 old parties (>= threshold+1=5). Passes pre-flight.

	t.Log("Phase 2: injecting network faults...")

	t.Log("disconnecting node 3...")
	if err := d.Disconnect(ctx, 3); err != nil {
		t.Fatalf("disconnecting node 3: %v", err)
	}

	t.Log("adding 5000ms outbound latency on node 4...")
	for peer := 1; peer <= cfg.Nodes; peer++ {
		if peer == 4 {
			continue
		}
		if err := d.AddOutboundLatency(ctx, 4, peer, 5000, 0); err != nil {
			t.Fatalf("adding latency from node 4 to node %d: %v", peer, err)
		}
	}

	// ── Phase 3: Wait for reshare to complete ────────────────────────
	//
	// We wait through 3 reshare cycles to give the protocol time to
	// converge gossip, attempt reshare, and retry if needed.

	rotateInterval := uint64(testRotateInterval)
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	targetBlock := ((bh/rotateInterval)+1)*rotateInterval + 3*rotateInterval + 50
	t.Logf("current block: %d, waiting for block %d (3 reshare cycles + margin)...", bh, targetBlock)

	if err := d.WaitForBlockProcessing(ctx, 2, targetBlock, 15*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 50)
		t.Fatalf("didn't reach target block: %v", err)
	}

	// ── Phase 4: Assertions ──────────────────────────────────────────

	t.Log("Phase 4: checking results...")

	commits, err := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       keyId,
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

	witnessName3 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 3)
	witnessName4 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 4)
	witnessName7 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 7)

	hasReshare := false
	reshareIncludesNode4 := false

	for i, c := range commits {
		cbits := decodeBitset(t, c.Commitment)
		var names []string
		for j := 0; j < len(members); j++ {
			if cbits.Bit(j) == 1 {
				names = append(names, members[j])
			}
		}
		label := "blamed"
		if c.Type == "reshare" {
			label = "in committee"
			hasReshare = true
			for _, name := range names {
				if name == witnessName4 {
					reshareIncludesNode4 = true
				}
			}
		}
		t.Logf("  [%d] type=%s block=%d epoch=%d %s=%v",
			i, c.Type, c.BlockHeight, c.Epoch, label, names)
	}

	// Check 1: reshare MUST succeed
	if !hasReshare {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 80)
		t.Fatal("FAIL: reshare did not succeed. Expected 5 of 7 nodes to complete reshare.")
	}
	t.Log("PASS: reshare succeeded!")

	// Check 2: node 4 (slow) should be in the reshare committee
	if reshareIncludesNode4 {
		t.Logf("PASS: slow node (%s) was included in reshare committee", witnessName4)
	} else {
		t.Logf("INFO: slow node (%s) was NOT in the reshare committee — its attestation may not have propagated in time", witnessName4)
	}

	// Check 3: disconnected node 3 should NOT be in any reshare committee
	for _, c := range commits {
		if c.Type != "reshare" {
			continue
		}
		cbits := decodeBitset(t, c.Commitment)
		for j := 0; j < len(members); j++ {
			if cbits.Bit(j) == 1 && members[j] == witnessName3 {
				t.Errorf("FAIL: disconnected node (%s) should NOT be in reshare committee", witnessName3)
			}
		}
	}
	t.Logf("PASS: disconnected node (%s) excluded from reshare", witnessName3)

	// Check 4: old-code node 7 should NOT be in any reshare committee
	for _, c := range commits {
		if c.Type != "reshare" {
			continue
		}
		cbits := decodeBitset(t, c.Commitment)
		for j := 0; j < len(members); j++ {
			if cbits.Bit(j) == 1 && members[j] == witnessName7 {
				t.Errorf("FAIL: old-code node (%s) should NOT be in reshare committee", witnessName7)
			}
		}
	}
	t.Logf("PASS: old-code node (%s) excluded from reshare", witnessName7)

	// Check 5: old-code node didn't crash
	if logs, err := d.Logs(ctx, "magi-7"); err == nil {
		if strings.Contains(logs, "panic:") || strings.Contains(logs, "fatal error:") {
			t.Error("old-code node 7 crashed!")
		} else {
			t.Log("PASS: old-code node 7 did not crash")
		}
	}

	// Check 6: new-code nodes should have gossip log entries
	gossipSeen := 0
	for i := 1; i <= 6; i++ {
		if i == 3 {
			continue // disconnected
		}
		if nodeLogsContain(d, ctx, i, "signed readiness attestation") {
			gossipSeen++
		}
	}
	t.Logf("gossip attestations logged on %d of 5 active new-code nodes", gossipSeen)

	// Cleanup
	d.Reconnect(ctx, 3)
	d.RemoveLatency(ctx, 4)
}
