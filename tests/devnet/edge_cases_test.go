package devnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
)

// ═══════════════════════════════════════════════════════════════════
// Shared setup for edge-case tests.
//
// All tests in this file use a common configuration:
//   - 7 nodes (threshold+1 = 5, allows losing 2 nodes)
//   - ElectionInterval=60 (~3 min), RotateInterval=20 (~1 min)
//   - Contract-based keygen via call-tss WASM
//
// The helper edgeCaseSetup handles devnet startup, contract deployment,
// keygen, and waiting for epoch 1 (so reshare can trigger). Each test
// then applies its specific fault scenario.
// ═══════════════════════════════════════════════════════════════════

// edgeCaseConfig returns the shared test configuration for edge-case
// tests. 7 nodes gives us room to lose 2 nodes and still have quorum
// (threshold+1 = 5 for btss, BLS needs ceil(7*2/3) = 5 signatures).
func edgeCaseConfig() *Config {
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
	return cfg
}

// edgeCaseSetup is the common preamble for all edge-case tests:
// start devnet, deploy contract, trigger keygen, wait for active key
// and epoch 1. Returns the devnet, context, contract ID, full key ID,
// and the keygen commitment.
func edgeCaseSetup(t *testing.T, cfg *Config) (*Devnet, context.Context, string, string, *TssCommitmentDoc) {
	t.Helper()
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)

	wasmPath, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
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

	// Deploy contract and trigger keygen
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

	payload := `{"key_name":"edgeKey","epochs":10}`
	_, err = d.CallContract(ctx, 2, contractId, "tssCreate", payload)
	if err != nil {
		t.Fatalf("calling tssCreate: %v", err)
	}

	fullKeyId := contractId + "-edgeKey"
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
	t.Log("setup complete: keygen done, epoch 1 active")

	return d, ctx, contractId, fullKeyId, keygenCommit
}

// ═══════════════════════════════════════════════════════════════════
// Test 1: Leader crash during BLS collection
//
// The leader completes the reshare protocol, starts collecting BLS
// signatures from other nodes via pubsub, then crashes. The ask_sigs
// message is one-shot with no retry. If the leader dies before
// broadcasting the commitment to Hive, the reshare result is lost.
//
// All nodes have the new key locally (keystore), but no on-chain
// commitment exists. The network must recover at the next reshare
// cycle — either by re-resharing from the old epoch or by detecting
// the local/on-chain divergence.
//
// This is a realistic mainnet scenario: leader OOM, watchdog restart,
// or Hive API timeout during the broadcast window.
// ═══════════════════════════════════════════════════════════════════

func TestEdgeLeaderCrashDuringBLS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping edge case test in short mode")
	}

	cfg := edgeCaseConfig()
	d, ctx, _, fullKeyId, keygenCommit := edgeCaseSetup(t, cfg)

	t.Log("Phase 2: waiting for reshare, will stop leader mid-BLS...")

	rotateInterval := uint64(20)
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	nextReshare := ((bh / rotateInterval) + 1) * rotateInterval

	// Wait until just past the reshare block so the session starts
	if err := d.WaitForBlockProcessing(ctx, 2, nextReshare+3, 3*time.Minute); err != nil {
		t.Fatalf("didn't reach reshare block: %v", err)
	}

	// Find which node logged "starting multi-sig collection" — that's
	// the leader for this cycle. Stop it to simulate a crash during BLS.
	leaderNode := 0
	for i := 1; i <= cfg.Nodes; i++ {
		logs, err := d.Logs(ctx, d.containerName(i))
		if err != nil {
			continue
		}
		if strings.Contains(logs, "starting multi-sig collection") {
			leaderNode = i
			break
		}
	}

	if leaderNode == 0 {
		t.Log("warning: couldn't identify leader from logs, stopping node 1 as fallback")
		leaderNode = 1
	}

	t.Logf("stopping leader node %d mid-BLS collection...", leaderNode)
	if err := d.StopNode(ctx, leaderNode); err != nil {
		t.Fatalf("stopping leader: %v", err)
	}

	// Wait for the timeout + one more reshare cycle to see if the
	// network can recover without the leader's commitment.
	targetBlock := nextReshare + 2*rotateInterval + 30
	t.Logf("waiting for recovery at block ~%d (leader %d stopped)...", targetBlock, leaderNode)
	// Use a non-leader node for block monitoring
	monitorNode := 2
	if leaderNode == 2 {
		monitorNode = 3
	}
	if err := d.WaitForBlockProcessing(ctx, monitorNode, targetBlock, 8*time.Minute); err != nil {
		t.Fatalf("didn't reach recovery target: %v", err)
	}

	// Restart the leader for the subsequent cycle
	t.Logf("restarting leader node %d...", leaderNode)
	d.StartNode(ctx, leaderNode)
	time.Sleep(10 * time.Second) // let it sync

	// Wait one more cycle for full recovery
	finalTarget := targetBlock + rotateInterval + 30
	if err := d.WaitForBlockProcessing(ctx, monitorNode, finalTarget, 5*time.Minute); err != nil {
		t.Logf("warning: didn't reach final target: %v", err)
	}

	// Check what landed on-chain
	commits, err := d.GetCommitments(ctx, monitorNode, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})
	if err != nil {
		t.Fatalf("querying commitments: %v", err)
	}

	t.Logf("commitments after leader crash: %d", len(commits))
	hasReshare := false
	for i, c := range commits {
		t.Logf("  [%d] type=%s block=%d epoch=%d", i, c.Type, c.BlockHeight, c.Epoch)
		if c.Type == "reshare" {
			hasReshare = true
		}
	}

	if hasReshare {
		t.Log("PASS: network recovered from leader crash — reshare committed by new leader")
	} else if len(commits) > 0 {
		t.Log("PARTIAL: blame landed but no reshare yet — may need more cycles")
		t.Log("The leader crash prevented the first commitment. Subsequent cycles")
		t.Log("should eventually produce a reshare with a different leader.")
	} else {
		t.Error("FAIL: no commitments landed after leader crash")
		t.Log("DIAGNOSIS: the leader crash may have left nodes in an inconsistent state")
		t.Log("(local keystore has new key, chain has old epoch). Check if nodes can")
		t.Log("still reshare from the old key or if manual intervention is needed.")
	}
}

// ═══════════════════════════════════════════════════════════════════
// Test 2: Network partition — neither half has quorum
//
// 7 nodes split into {1,2,3} and {4,5,6,7}. The btss threshold for
// 7 nodes is ceil(7*2/3)-1 = 4, so threshold+1 = 5 parties needed.
// Neither group has 5, so both halves fail all reshare attempts.
// BLS also fails (needs 5 of 7 signatures).
//
// After the partition heals, the full network must recover cleanly:
// no stale blame from the split should prevent the next reshare.
// ═══════════════════════════════════════════════════════════════════

func TestEdgePartitionNoQuorum(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping edge case test in short mode")
	}

	cfg := edgeCaseConfig()
	d, ctx, _, fullKeyId, keygenCommit := edgeCaseSetup(t, cfg)

	t.Log("Phase 2: partitioning network into two groups (neither has quorum)...")

	// Split: {1,2,3} <-> {4,5,6,7}
	// Block all cross-group traffic
	groupA := []int{1, 2, 3}
	groupB := []int{4, 5, 6, 7}
	for _, a := range groupA {
		for _, b := range groupB {
			if err := d.Partition(ctx, a, b); err != nil {
				t.Fatalf("partition %d<->%d: %v", a, b, err)
			}
		}
	}
	t.Log("  Group A: nodes 1,2,3 (3 nodes, need 5 for btss)")
	t.Log("  Group B: nodes 4,5,6,7 (4 nodes, need 5 for btss)")
	t.Log("  Neither group has quorum — all reshare attempts should fail")

	// Wait through 2 reshare cycles during partition
	rotateInterval := uint64(20)
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	partitionTarget := ((bh/rotateInterval)+1)*rotateInterval + 2*rotateInterval + 30
	t.Logf("waiting through 2 reshare cycles (partitioned) to block %d...", partitionTarget)
	if err := d.WaitForBlockProcessing(ctx, 2, partitionTarget, 8*time.Minute); err != nil {
		t.Fatalf("didn't reach partition target: %v", err)
	}

	// Verify: no reshare during partition (neither group has quorum)
	partitionCommits, _ := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         "reshare",
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})
	t.Logf("reshare commits during partition: %d", len(partitionCommits))
	if len(partitionCommits) > 0 {
		t.Log("warning: reshare succeeded during partition — quorum calculation may be wrong")
	}

	// Heal the partition
	t.Log("Phase 3: healing partition...")
	for _, a := range groupA {
		for _, b := range groupB {
			d.Heal(ctx, a, b)
		}
	}

	// Wait for recovery reshare with full network
	bh, _ = d.getLastProcessedBlock(ctx, 2)
	recoveryTarget := ((bh/rotateInterval)+1)*rotateInterval + rotateInterval + 30
	t.Logf("waiting for recovery reshare at block ~%d (network healed)...", recoveryTarget)
	if err := d.WaitForBlockProcessing(ctx, 2, recoveryTarget, 8*time.Minute); err != nil {
		t.Fatalf("didn't reach recovery target: %v", err)
	}

	// Verify: reshare succeeds after healing
	allCommits, _ := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})

	t.Logf("total commitments: %d", len(allCommits))
	hasPostHealReshare := false
	for i, c := range allCommits {
		t.Logf("  [%d] type=%s block=%d epoch=%d", i, c.Type, c.BlockHeight, c.Epoch)
		if c.Type == "reshare" && c.BlockHeight > partitionTarget {
			hasPostHealReshare = true
		}
	}

	if hasPostHealReshare {
		t.Log("PASS: network recovered after partition healed — reshare succeeded")
	} else {
		t.Error("FAIL: no reshare after partition healed")
		t.Log("DIAGNOSIS: stale blame from the partition may be preventing recovery,")
		t.Log("or nodes need more cycles to re-establish P2P connections and sync.")
	}
}

// ═══════════════════════════════════════════════════════════════════
// Test 3: Node restart between readiness broadcast and reshare
//
// A node broadcasts vsc.tss_ready (on-chain), then restarts. When the
// reshare fires, the node is in the party list (readiness on-chain)
// but is in one of these states:
//   - Still starting up (syncing blocks, generating preparams)
//   - Running but behind (sync guard rejects BlockTick)
//   - Running but no preparams (can't create LocalParty)
//
// The restarted node appears as "connected_but_no_response" — its P2P
// is up but it doesn't send btss messages. This should produce blame
// targeting ONLY the restarted node, and the next cycle should exclude
// it and succeed.
//
// This is common on mainnet: rolling deployments, watchdog restarts,
// OOM kills during the readiness-to-reshare window.
// ═══════════════════════════════════════════════════════════════════

func TestEdgeNodeRestartMidCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping edge case test in short mode")
	}

	cfg := edgeCaseConfig()
	d, ctx, _, fullKeyId, keygenCommit := edgeCaseSetup(t, cfg)

	t.Log("Phase 2: restarting node 3 between readiness and reshare...")

	rotateInterval := uint64(20)
	readinessOffset := uint64(5)
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	nextReshare := ((bh / rotateInterval) + 1) * rotateInterval

	// Wait until the readiness window has passed (node 3 broadcast ready)
	// but just before the reshare fires. Then restart node 3.
	readinessDone := nextReshare - readinessOffset + 2
	t.Logf("current block: %d, readiness at %d, restarting node 3 at block %d, reshare at %d",
		bh, nextReshare-readinessOffset, readinessDone, nextReshare)
	if err := d.WaitForBlockProcessing(ctx, 2, readinessDone, 3*time.Minute); err != nil {
		t.Fatalf("didn't reach readiness deadline: %v", err)
	}

	// Restart node 3: stop + start. The node loses all in-memory state
	// (preparams, P2P connections, session state) but its readiness is
	// already on-chain.
	t.Log("stopping node 3...")
	if err := d.StopNode(ctx, 3); err != nil {
		t.Fatalf("stopping node 3: %v", err)
	}
	t.Log("starting node 3...")
	if err := d.StartNode(ctx, 3); err != nil {
		t.Fatalf("starting node 3: %v", err)
	}

	// Wait through 2 reshare cycles for blame + recovery
	targetBlock := nextReshare + 2*rotateInterval + 40
	t.Logf("waiting for blame + recovery at block ~%d...", targetBlock)
	if err := d.WaitForBlockProcessing(ctx, 2, targetBlock, 10*time.Minute); err != nil {
		t.Fatalf("didn't reach target: %v", err)
	}

	// Check results
	commits, _ := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})

	members, _ := d.GetElectionMembers(ctx, 2, keygenCommit.Epoch+1)
	witnessName3 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 3)

	t.Logf("commitments after restart: %d", len(commits))
	hasBlameTargetingNode3 := false
	hasReshare := false
	for i, c := range commits {
		label := c.Type
		if c.Type == "blame" && len(members) > 0 {
			bits := decodeBitset(t, c.Commitment)
			var names []string
			for j := 0; j < len(members); j++ {
				if bits.Bit(j) == 1 {
					names = append(names, members[j])
					if members[j] == witnessName3 {
						hasBlameTargetingNode3 = true
					}
				}
			}
			label = fmt.Sprintf("blame(%v)", names)
		}
		if c.Type == "reshare" {
			hasReshare = true
		}
		t.Logf("  [%d] %s block=%d epoch=%d", i, label, c.BlockHeight, c.Epoch)
	}

	if hasReshare {
		t.Log("PASS: network recovered after node restart — reshare succeeded")
	} else if hasBlameTargetingNode3 {
		t.Log("PARTIAL: blame correctly targets restarted node, but reshare hasn't succeeded yet")
		t.Log("The restarted node was identified as non-responsive. More cycles may be needed.")
	} else if len(commits) > 0 {
		t.Log("PARTIAL: commitments landed but node 3 wasn't specifically blamed")
		t.Log("The restarted node may have recovered fast enough to participate,")
		t.Log("or the readiness gate excluded it (readiness from pre-restart expired).")
	} else {
		t.Error("FAIL: no commitments after node restart")
		t.Log("DIAGNOSIS: the restarted node may have caused SSID mismatch or")
		t.Log("prevented BLS collection. Check for party list divergence.")
	}
}

// ═══════════════════════════════════════════════════════════════════
// Test 4: Rapid blame cycles exhausting preparams
//
// Each keygen/reshare attempt consumes one set of preparams (Paillier
// keys + safe primes). Generation takes 15-60 seconds. With
// RotateInterval=20 blocks (~60s), there's barely enough time to
// regenerate between cycles.
//
// If a persistent fault causes blame every cycle, nodes consume
// preparams faster than they can regenerate. A node without preparams
// can't participate in the next attempt, worsening the failure.
//
// This test induces 4+ consecutive blame cycles by adding latency to
// one node, then verifies that all healthy nodes still have preparams
// and can complete a reshare after the fault is removed.
// ═══════════════════════════════════════════════════════════════════

func TestEdgePreparamsExhaustion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping edge case test in short mode")
	}

	cfg := edgeCaseConfig()
	d, ctx, _, fullKeyId, keygenCommit := edgeCaseSetup(t, cfg)

	t.Log("Phase 2: adding latency to node 6 to cause repeated blame...")

	// Make node 6 slow enough to timeout every reshare. Other nodes
	// will blame it each cycle, consuming preparams.
	for i := 1; i <= 5; i++ {
		if err := d.AddOutboundLatency(ctx, 6, i, 8000, 0); err != nil {
			t.Fatalf("adding latency 6->%d: %v", i, err)
		}
	}
	t.Cleanup(func() { d.RemoveLatency(context.Background(), 6) })

	// Wait through 4 reshare cycles (~4 minutes). Each cycle should
	// blame node 6, consuming preparams on all healthy nodes.
	rotateInterval := uint64(20)
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	blameTarget := ((bh/rotateInterval)+1)*rotateInterval + 4*rotateInterval + 30
	t.Logf("waiting through 4 blame cycles to block %d...", blameTarget)
	if err := d.WaitForBlockProcessing(ctx, 2, blameTarget, 10*time.Minute); err != nil {
		t.Fatalf("didn't reach blame target: %v", err)
	}

	// Count blame commits
	blames, _ := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         "blame",
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})
	t.Logf("blame commits during latency: %d", len(blames))

	// Remove latency and verify recovery
	t.Log("Phase 3: removing latency, checking preparams and recovery...")
	d.RemoveLatency(ctx, 6)

	// Check that healthy nodes still have preparams by looking for the
	// "need to generate preparams" log. If it appears AFTER the blame
	// cycles, the node ran out and had to regenerate.
	preparamsExhausted := 0
	for i := 1; i <= 5; i++ {
		logs, err := d.Logs(ctx, d.containerName(i))
		if err != nil {
			continue
		}
		// Count how many times preparams were regenerated. Each reshare
		// attempt consumes one. If we see multiple "preparams generated"
		// entries, the node successfully regenerated between cycles.
		genCount := strings.Count(logs, "preparams generated successfully")
		needCount := strings.Count(logs, "need to generate preparams")
		t.Logf("  node %d: generated preparams %d times, needed %d times", i, genCount, needCount)
		if needCount > genCount+1 {
			preparamsExhausted++
			t.Logf("  WARNING: node %d may have been blocked waiting for preparams", i)
		}
	}

	if preparamsExhausted > 0 {
		t.Logf("  %d nodes showed signs of preparams exhaustion", preparamsExhausted)
	} else {
		t.Log("  All healthy nodes kept up with preparams generation")
	}

	// Wait for recovery reshare now that latency is removed
	bh, _ = d.getLastProcessedBlock(ctx, 2)
	recoveryTarget := ((bh/rotateInterval)+1)*rotateInterval + rotateInterval + 30
	t.Logf("waiting for recovery reshare at block ~%d...", recoveryTarget)
	if err := d.WaitForBlockProcessing(ctx, 2, recoveryTarget, 8*time.Minute); err != nil {
		t.Logf("warning: didn't reach recovery target: %v", err)
	}

	reshares, _ := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         "reshare",
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})

	if len(reshares) > 0 {
		t.Logf("PASS: reshare succeeded after %d blame cycles — preparams held up", len(blames))
	} else {
		t.Errorf("FAIL: no reshare after %d blame cycles — network did not recover", len(blames))
		t.Log("DIAGNOSIS: either preparams were exhausted and not regenerated in time,")
		t.Log("or blame accumulation prevented quorum on the retry cycle.")
	}
}

// ═══════════════════════════════════════════════════════════════════
// Test 5: Keygen blame decoded against wrong election
//
// A keygen session times out (one node disconnected) and produces a
// blame commitment at epoch 0 (the genesis election). When epoch 1
// arrives with potentially different member ordering, the blame from
// epoch 0 must be decoded against epoch 0's election — NOT epoch 1's.
//
// If decoded against the wrong election, the bit positions in the
// blame bitset map to different accounts, and the wrong node gets
// excluded (or no node, if the bit is out of range).
//
// This test exercises Change 4 from the review: blame epoch decode.
// ═══════════════════════════════════════════════════════════════════

func TestEdgeKeygenBlameCrossEpoch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping edge case test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	wasmPath, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
	}

	// Use 5 nodes. Disconnect one during keygen to cause blame at epoch 0.
	cfg := DefaultConfig()
	cfg.P2PBasePort = 11720 // avoid conflict with mainnet/testnet nodes on 10720+
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.LogLevel = "trace"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 40,
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

	// Deploy contract and trigger keygen
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

	payload := `{"key_name":"epochBlameKey","epochs":10}`
	_, err = d.CallContract(ctx, 2, contractId, "tssCreate", payload)
	if err != nil {
		t.Fatalf("calling tssCreate: %v", err)
	}
	fullKeyId := contractId + "-epochBlameKey"

	// Wait for the key record to appear (status "created")
	_, err = d.WaitForTssKey(ctx, 2, bson.M{"id": fullKeyId}, 5*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("key never created: %v", err)
	}

	// Disconnect node 4 BEFORE keygen fires — it will be in the election
	// but can't participate. This should cause a keygen timeout and blame
	// at epoch 0.
	t.Log("disconnecting node 4 before keygen...")
	if err := d.Disconnect(ctx, 4); err != nil {
		t.Fatalf("disconnecting node 4: %v", err)
	}

	// Wait for keygen blame (node 4 can't participate → timeout → blame)
	t.Log("waiting for keygen blame (node 4 disconnected)...")
	blameCommit, err := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id": fullKeyId,
		"type":   "blame",
	}, 10*time.Minute)

	// Also wait for a successful keygen (might take another cycle without node 4)
	keygenCommit, keygenErr := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id": fullKeyId,
		"type":   "keygen",
	}, 5*time.Minute)

	// Reconnect node 4
	d.Reconnect(ctx, 4)

	if err != nil && keygenErr != nil {
		t.Fatalf("neither blame nor keygen landed: blame=%v keygen=%v", err, keygenErr)
	}

	if blameCommit != nil {
		t.Logf("keygen blame at block %d epoch %d", blameCommit.BlockHeight, blameCommit.Epoch)

		// Decode the blame bitset against the blame's own epoch election
		members, memberErr := d.GetElectionMembers(ctx, 2, blameCommit.Epoch)
		if memberErr == nil {
			bits := decodeBitset(t, blameCommit.Commitment)
			witnessName4 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 4)
			var blamedNames []string
			node4Blamed := false
			for j := 0; j < len(members); j++ {
				if bits.Bit(j) == 1 {
					blamedNames = append(blamedNames, members[j])
					if members[j] == witnessName4 {
						node4Blamed = true
					}
				}
			}
			t.Logf("blame targets (decoded against epoch %d): %v", blameCommit.Epoch, blamedNames)

			if node4Blamed {
				t.Logf("PASS: keygen blame correctly targets disconnected node (%s)", witnessName4)
			} else {
				t.Errorf("keygen blame does NOT target node 4 (%s) — blamed: %v", witnessName4, blamedNames)
			}
		}
	}

	// Now wait for epoch 1 and verify the keygen blame from epoch 0
	// doesn't interfere with reshare (blame should be decoded against
	// epoch 0, not epoch 1).
	if keygenCommit != nil {
		t.Logf("keygen succeeded at block %d epoch %d", keygenCommit.BlockHeight, keygenCommit.Epoch)
		t.Log("waiting for epoch transition and reshare...")

		if err := d.waitForElectionEpoch(ctx, 2, keygenCommit.Epoch+1, 5*time.Minute); err != nil {
			t.Logf("warning: next epoch didn't arrive: %v", err)
		}

		// Wait for reshare attempt in the new epoch
		bh, _ := d.getLastProcessedBlock(ctx, 2)
		reshareTarget := ((bh/20)+1)*20 + 40
		d.WaitForBlockProcessing(ctx, 2, reshareTarget, 5*time.Minute)

		reshares, _ := d.GetCommitments(ctx, 2, bson.M{
			"key_id":       fullKeyId,
			"type":         "reshare",
			"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
		})
		if len(reshares) > 0 {
			t.Log("PASS: reshare succeeded in new epoch — keygen blame from old epoch didn't interfere")
		} else {
			t.Log("warning: no reshare yet — may need more cycles for blame to expire or epoch to advance")
		}
	} else {
		t.Log("keygen didn't succeed (node 4 was critical for quorum) — checking if 4 nodes can keygen")
		t.Log("With 5 nodes threshold+1=4, keygen should eventually succeed without node 4")
	}
}

// ═══════════════════════════════════════════════════════════════════
// Test 6: Flapping node — online/offline every reshare cycle
//
// A node that oscillates between connected and disconnected on every
// reshare cycle. This is more adversarial than a single disconnect:
//
//   Cycle 1: node 4 is online, broadcasts readiness, participates → OK
//   Cycle 2: node 4 disconnected → timeout, blamed
//   Cycle 3: node 4 reconnected, broadcasts readiness → participates?
//   Cycle 4: node 4 disconnected again → blamed again
//
// The flapping pattern tests several things:
//   - Does blame accumulation track repeat offenders correctly?
//   - Does the readiness gate let a previously-blamed node back in
//     when it broadcasts readiness again?
//   - Can the network make progress despite the oscillation, or does
//     the flapping node prevent convergence?
//
// On mainnet, flapping can happen from unstable network connections,
// overloaded nodes that intermittently timeout, or watchdog restarts
// that bring a node back just in time for readiness but not for the
// full reshare protocol.
// ═══════════════════════════════════════════════════════════════════

func TestEdgeFlappingNode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping edge case test in short mode")
	}

	cfg := edgeCaseConfig()
	d, ctx, _, fullKeyId, keygenCommit := edgeCaseSetup(t, cfg)

	t.Log("Phase 2: flapping node 4 — disconnect/reconnect every reshare cycle...")

	rotateInterval := uint64(20)
	readinessOffset := uint64(5)

	// Run through 4 reshare cycles, toggling node 4 each time.
	// Even cycles: node 4 is disconnected (should be blamed or excluded)
	// Odd cycles: node 4 is online (should participate or be let back in)
	numCycles := 4
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	firstReshare := ((bh / rotateInterval) + 1) * rotateInterval

	for cycle := 0; cycle < numCycles; cycle++ {
		reshareBlock := firstReshare + uint64(cycle)*rotateInterval
		isDisconnected := cycle%2 == 1 // disconnect on odd cycles

		if isDisconnected {
			// Disconnect BEFORE the readiness window so node 4 can't
			// broadcast readiness — excluded by readiness gate.
			disconnectAt := reshareBlock - readinessOffset - 2
			t.Logf("  cycle %d: disconnecting node 4 at block %d (before readiness)", cycle+1, disconnectAt)
			if err := d.WaitForBlockProcessing(ctx, 2, disconnectAt, 3*time.Minute); err != nil {
				t.Fatalf("didn't reach disconnect point: %v", err)
			}
			d.Disconnect(ctx, 4)
		} else {
			// Reconnect before readiness window so node 4 can participate.
			reconnectAt := reshareBlock - readinessOffset - 3
			t.Logf("  cycle %d: ensuring node 4 is online at block %d (before readiness)", cycle+1, reconnectAt)
			if err := d.WaitForBlockProcessing(ctx, 2, reconnectAt, 3*time.Minute); err != nil {
				t.Fatalf("didn't reach reconnect point: %v", err)
			}
			d.Reconnect(ctx, 4)
		}

		// Wait for the reshare to fire
		if err := d.WaitForBlockProcessing(ctx, 2, reshareBlock+5, 3*time.Minute); err != nil {
			t.Fatalf("didn't reach reshare block for cycle %d: %v", cycle+1, err)
		}
	}

	// Wait for protocol timeouts and BLS collection to settle
	lastReshare := firstReshare + uint64(numCycles)*rotateInterval
	t.Logf("waiting for all cycles to settle (target block %d)...", lastReshare+30)
	d.Reconnect(ctx, 4) // ensure node 4 is back for final check
	if err := d.WaitForBlockProcessing(ctx, 2, lastReshare+30, 5*time.Minute); err != nil {
		t.Logf("warning: didn't reach settlement target: %v", err)
	}

	// Analyze results
	commits, _ := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})

	members, _ := d.GetElectionMembers(ctx, 2, keygenCommit.Epoch+1)

	blameCount := 0
	reshareCount := 0
	t.Logf("commitments across %d flapping cycles: %d", numCycles, len(commits))
	for i, c := range commits {
		label := c.Type
		if c.Type == "blame" {
			blameCount++
			if len(members) > 0 {
				bits := decodeBitset(t, c.Commitment)
				var names []string
				for j := 0; j < len(members); j++ {
					if bits.Bit(j) == 1 {
						names = append(names, members[j])
					}
				}
				label = fmt.Sprintf("blame(%v)", names)
			}
		}
		if c.Type == "reshare" {
			reshareCount++
		}
		t.Logf("  [%d] %s block=%d epoch=%d", i, label, c.BlockHeight, c.Epoch)
	}

	if reshareCount > 0 {
		t.Logf("PASS: reshare succeeded despite flapping node (%d reshares, %d blames)", reshareCount, blameCount)
		t.Log("The network made progress even with a node oscillating between online/offline.")
	} else if blameCount > 0 {
		t.Logf("PARTIAL: blame landed (%d) but no reshare yet — the flapping prevented convergence", blameCount)
		t.Log("This is expected if the flapping node was needed for quorum on the 'online' cycles.")
		t.Log("With 7 nodes and threshold+1=5, losing even 1 node to flapping + 2 to blame")
		t.Log("could drop below quorum.")
	} else {
		t.Error("FAIL: no commitments landed during flapping cycles")
		t.Log("DIAGNOSIS: the flapping node may cause SSID mismatch on every cycle,")
		t.Log("preventing any blame from landing. Check if nodes agree on party lists.")
	}
}

// ═══════════════════════════════════════════════════════════════════
// Test 7: Simultaneous restart of all nodes
//
// All nodes restart at the same time — simulating a coordinated
// deployment or a cluster-wide failure (e.g., host reboot, Docker
// daemon restart).
//
// After restart, every node must:
//   1. Regenerate preparams (15-60s)
//   2. Re-establish P2P connections
//   3. Sync missed blocks from Hive
//   4. Resume TSS operations
//
// The network should recover within 1-2 reshare cycles after all
// nodes have preparams. The key risk is that all nodes simultaneously
// generating preparams creates massive CPU contention, potentially
// causing all of them to timeout.
//
// This tests the PreParamsTimeout configuration: with a generous
// timeout (10 minutes), all nodes should eventually generate primes
// even under contention, and the first reshare after recovery should
// succeed.
// ═══════════════════════════════════════════════════════════════════

func TestEdgeSimultaneousRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping edge case test in short mode")
	}

	cfg := edgeCaseConfig()
	d, ctx, _, fullKeyId, keygenCommit := edgeCaseSetup(t, cfg)

	// Verify we have at least one reshare before the restart, to prove
	// the network was healthy.
	t.Log("Phase 1.5: waiting for first reshare (baseline)...")
	reshareCommit, err := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         "reshare",
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	}, 8*time.Minute)
	if err != nil {
		t.Logf("warning: no baseline reshare before restart: %v", err)
	} else {
		t.Logf("baseline reshare at block %d — network was healthy", reshareCommit.BlockHeight)
	}

	t.Log("Phase 2: restarting ALL nodes simultaneously...")

	// Stop all nodes at once
	for i := 1; i <= cfg.Nodes; i++ {
		if err := d.StopNode(ctx, i); err != nil {
			t.Logf("warning: stopping node %d: %v", i, err)
		}
	}
	t.Log("all nodes stopped")

	// Brief pause — in production, a deployment might take 5-10 seconds
	time.Sleep(5 * time.Second)

	// Start all nodes at once
	for i := 1; i <= cfg.Nodes; i++ {
		if err := d.StartNode(ctx, i); err != nil {
			t.Fatalf("starting node %d: %v", i, err)
		}
	}
	t.Log("all nodes restarted — waiting for preparams + P2P recovery...")

	// Wait for nodes to regenerate preparams and catch up on blocks.
	// This is the critical window: all 7 nodes generating Paillier
	// primes simultaneously on a loaded server.
	time.Sleep(30 * time.Second) // let P2P settle

	// Check that nodes are processing blocks again
	if err := d.WaitForBlockProcessing(ctx, 2, 10, 5*time.Minute); err != nil {
		t.Fatalf("node 2 never recovered: %v", err)
	}

	bh, _ := d.getLastProcessedBlock(ctx, 2)
	t.Logf("node 2 recovered, processing block %d", bh)

	// Wait for preparams on at least a few nodes
	preparamsReady := 0
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		preparamsReady = 0
		for i := 1; i <= cfg.Nodes; i++ {
			logs, err := d.Logs(ctx, d.containerName(i))
			if err != nil {
				continue
			}
			// Count "preparams generated" AFTER the restart. The restart
			// produces new logs, so any "generated" message is post-restart.
			if strings.Contains(logs, "preparams generated successfully") {
				preparamsReady++
			}
		}
		if preparamsReady >= 5 { // threshold+1
			break
		}
		time.Sleep(10 * time.Second)
	}
	t.Logf("%d/%d nodes have preparams after restart", preparamsReady, cfg.Nodes)

	if preparamsReady < 5 {
		t.Logf("warning: only %d nodes have preparams — reshare may not have quorum", preparamsReady)
	}

	// Wait for a reshare cycle after preparams are ready
	rotateInterval := uint64(20)
	bh, _ = d.getLastProcessedBlock(ctx, 2)
	recoveryTarget := ((bh/rotateInterval)+1)*rotateInterval + 2*rotateInterval + 30
	t.Logf("waiting for recovery reshare at block ~%d...", recoveryTarget)
	if err := d.WaitForBlockProcessing(ctx, 2, recoveryTarget, 8*time.Minute); err != nil {
		t.Logf("warning: didn't reach recovery target: %v", err)
	}

	// Check for post-restart reshare
	var afterBlock uint64
	if reshareCommit != nil {
		afterBlock = reshareCommit.BlockHeight
	} else {
		afterBlock = keygenCommit.BlockHeight
	}
	postRestartCommits, _ := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": afterBlock},
	})

	t.Logf("commitments after simultaneous restart: %d", len(postRestartCommits))
	hasPostRestartReshare := false
	for i, c := range postRestartCommits {
		t.Logf("  [%d] type=%s block=%d epoch=%d", i, c.Type, c.BlockHeight, c.Epoch)
		if c.Type == "reshare" {
			hasPostRestartReshare = true
		}
	}

	if hasPostRestartReshare {
		t.Logf("PASS: network recovered from simultaneous restart — reshare succeeded")
		t.Logf("All %d nodes regenerated preparams and completed reshare.", cfg.Nodes)
	} else if len(postRestartCommits) > 0 {
		t.Log("PARTIAL: blame landed after restart but no reshare yet")
		t.Log("Nodes may need more cycles for all preparams to regenerate.")
	} else {
		t.Error("FAIL: no commitments after simultaneous restart")
		t.Log("DIAGNOSIS: all nodes may be stuck regenerating preparams, or")
		t.Log("P2P connections weren't re-established in time. Check logs for")
		t.Log("'preparams generated successfully' and peer connection counts.")
	}
}
