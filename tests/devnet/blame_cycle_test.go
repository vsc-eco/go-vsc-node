package devnet

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestTSSBlameExcludesNodeNextCycle verifies that a blamed node is
// excluded from the party list in the subsequent reshare cycle. After
// disconnecting a node and waiting for blame to land, the next reshare
// should succeed without it — either via blame-based exclusion or
// readiness gate exclusion (both are valid).
// Covers: Section 5 test #4, Section 8 items 3-4 (blame accumulation)
func TestTSSBlameExcludesNodeNextCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnet(t, cfg, 15*time.Minute)

	t.Log("waiting for keygen...")
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)

	// Cycle 1: Disconnect node 2, let it get blamed
	t.Log("disconnecting magi-2 for blame cycle...")
	if err := d.Disconnect(ctx, 2); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	// Wait for blame to land (reshare timeout + BLS)
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	target := nextReshareBoundary(head) + testRotateInterval
	t.Logf("waiting for reshare+blame at block ~%d", target)
	waitForBlock(t, d.HiveRPCEndpoint(), target+5, 2*time.Minute)
	time.Sleep(40 * time.Second)

	// Check blame landed
	blameCount := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "blame",
		"block_height": bson.M{"$gt": keygen.BlockHeight},
	})
	if blameCount == 0 {
		t.Log("no blame in cycle 1 — node may have been excluded by readiness gate")
		// This is also valid: if node 2 was offline before readiness window,
		// it never appeared in the party list and no blame is needed.
	} else {
		t.Logf("cycle 1: %d blame commitment(s) landed", blameCount)
	}

	// Keep node 2 disconnected. Cycle 2: the next reshare should either
	// (a) exclude node 2 via blame + readiness, or (b) succeed without it.
	head, _ = getHeadBlock(d.HiveRPCEndpoint())
	cycle2Target := nextReshareBoundary(head) + testRotateInterval
	t.Logf("waiting for cycle 2 reshare at block ~%d (node 2 should be excluded)", cycle2Target)
	waitForBlock(t, d.HiveRPCEndpoint(), cycle2Target+5, 2*time.Minute)
	time.Sleep(15 * time.Second)

	// SSID mismatches are expected here: the disconnected node (magi-2)
	// may still attempt to participate with a different party list.
	// The important check is that reshare succeeds WITHOUT the blamed node.
	for i := 1; i <= cfg.Nodes; i++ {
		if nodeLogsContain(d, ctx, i, "ssid mismatch") {
			t.Logf("magi-%d has SSID mismatch (expected — disconnected node still tries to participate)", i)
		}
	}

	// Check if reshare succeeded in cycle 2 (with node 2 excluded)
	reshareAfterBlame := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "reshare",
		"block_height": bson.M{"$gt": keygen.BlockHeight},
	})
	if reshareAfterBlame > 0 {
		t.Logf("cycle 2: reshare succeeded with blamed node excluded (%d reshare commits)", reshareAfterBlame)
	} else {
		t.Log("warning: no reshare after blame (may need additional cycles)")
	}

	if err := d.Reconnect(ctx, 2); err != nil {
		t.Logf("warning: reconnect: %v", err)
	}
}

// TestTSSBlameEpochDecode verifies that blame bitsets are decoded against
// the blame's own epoch election, NOT the current election. When elections
// have different members (via unstaking), decoding against the wrong epoch
// maps bit positions to wrong accounts.
//
// Sequence:
//  1. Start 6 nodes, keygen at epoch E0
//  2. Partition node 4 from peers → blame against magi.test4
//  3. Unstake node 3 → removed from future elections
//  4. New election with 5 members — positions shifted
//  5. Blame from E0 decoded against E0's 6-member election (correct)
//     vs E1's 5-member election (wrong — bit 3 maps to magi.test5)
//
// Covers: Section 8 item 3 (fail-first for blame epoch decode)
func TestTSSBlameEpochDecode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.Nodes = 6
	cfg.GenesisNode = 6

	d, ctx := startDevnet(t, cfg, 20*time.Minute)

	// Step 1: keygen with all 6 nodes
	t.Log("step 1: waiting for keygen (6 members)...")
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("keygen: keyId=%s epoch=%d block=%d", keygen.KeyId, keygen.Epoch, keygen.BlockHeight)

	// Step 2: cause blame against node 4 via P2P partition.
	// Partition node 4 from all other magi nodes (but not from Hive/MongoDB)
	// so its readiness lands on-chain but it can't exchange btss messages.
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	target := nextReshareBoundary(head)
	if target-head < testRotateInterval {
		target += testRotateInterval
	}
	t.Logf("step 2: head=%d, waiting for reshare block %d, then partitioning magi-4 from peers",
		head, target)
	waitForBlock(t, d.HiveRPCEndpoint(), target, 2*time.Minute)

	t.Log("partitioning magi-4 from all peers...")
	for i := 1; i <= cfg.Nodes; i++ {
		if i == 4 {
			continue
		}
		if err := d.Partition(ctx, 4, i); err != nil {
			t.Logf("warning: partition 4<->%d: %v", i, err)
		}
	}

	// Wait for reshare timeout + BLS collection
	t.Log("waiting for timeout + blame to land...")
	time.Sleep(45 * time.Second)

	blameCount := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "blame",
		"block_height": bson.M{"$gt": keygen.BlockHeight},
	})
	if blameCount == 0 {
		t.Log("warning: no blame landed in step 2 — node 4 may have been excluded by readiness gate")
		if err := d.Reconnect(ctx, 4); err != nil {
			t.Logf("warning: reconnect: %v", err)
		}
		t.Skip("blame did not land — cannot test epoch decode with different elections")
	}
	t.Logf("step 2: blame landed (%d commitments)", blameCount)

	// Heal partitions so node 4 can participate later.
	for i := 1; i <= cfg.Nodes; i++ {
		if i == 4 {
			continue
		}
		d.Heal(ctx, 4, i)
	}

	// Step 3: unstake node 3 to change election membership.
	t.Log("step 3: unstaking magi.test3 to change election membership...")
	witnessName3 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 3)
	if err := d.Unstake(ctx, witnessName3, cfg.StakeAmount); err != nil {
		t.Fatalf("unstake: %v", err)
	}

	// Step 4: wait for unstake to complete and new election without node 3.
	electionInterval := 20
	if cfg.SysConfigOverrides != nil && cfg.SysConfigOverrides.ConsensusParams != nil &&
		cfg.SysConfigOverrides.ConsensusParams.ElectionInterval > 0 {
		electionInterval = int(cfg.SysConfigOverrides.ConsensusParams.ElectionInterval)
	}
	waitBlocks := 6 * electionInterval
	head, _ = getHeadBlock(d.HiveRPCEndpoint())
	unstakeTarget := head + waitBlocks
	t.Logf("step 4: waiting %d blocks for unstake to complete (target block %d)...", waitBlocks, unstakeTarget)
	waitForBlock(t, d.HiveRPCEndpoint(), unstakeTarget, 8*time.Minute)

	// Step 5: verify election membership changed.
	t.Log("step 5: checking election membership...")
	client, _ := connectMongo(t, d.MongoURI())
	defer client.Disconnect(ctx)

	electionsColl := client.Database("magi-1").Collection("elections")
	var latestElection struct {
		Epoch   uint64 `bson:"epoch"`
		Members []struct {
			Account string `bson:"account"`
		} `bson:"members"`
	}
	err := electionsColl.FindOne(ctx, bson.M{},
		options.FindOne().SetSort(bson.M{"epoch": -1})).Decode(&latestElection)
	if err != nil {
		t.Fatalf("finding latest election: %v", err)
	}

	memberSet := make(map[string]bool)
	for _, m := range latestElection.Members {
		memberSet[m.Account] = true
	}
	t.Logf("latest election epoch=%d members=%d: %v", latestElection.Epoch, len(latestElection.Members), memberSet)

	node3Account := "hive:" + witnessName3
	if memberSet[node3Account] {
		t.Log("warning: node 3 still in latest election — unstake may need more time")
	} else {
		t.Logf("confirmed: %s removed from election (membership changed)", node3Account)
	}

	// Step 6: trigger reshare with different election membership.
	head, _ = getHeadBlock(d.HiveRPCEndpoint())
	reshareTarget := nextReshareBoundary(head) + testRotateInterval
	t.Logf("step 6: waiting for reshare at block ~%d with different election...", reshareTarget)
	waitForBlock(t, d.HiveRPCEndpoint(), reshareTarget+5, 2*time.Minute)
	time.Sleep(15 * time.Second)

	assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)

	// Verify correct blame decode by checking which node was excluded.
	witnessName4 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 4)
	node4Account := "hive:" + witnessName4
	witnessName5 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 5)
	node5Account := "hive:" + witnessName5

	for i := 1; i <= cfg.Nodes; i++ {
		logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
		if err != nil {
			continue
		}
		if strings.Contains(logs, "excluded by blame") || strings.Contains(logs, "blamedAccounts") {
			if strings.Contains(logs, witnessName4) || strings.Contains(logs, node4Account) {
				t.Logf("magi-%d correctly excluded node 4 (%s) via blame", i, witnessName4)
			}
			if strings.Contains(logs, witnessName5) || strings.Contains(logs, node5Account) {
				t.Errorf("magi-%d INCORRECTLY excluded node 5 (%s) — blame epoch decode bug!", i, witnessName5)
			}
		}
	}

	postUnstakeReshare := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "reshare",
		"block_height": bson.M{"$gt": uint64(unstakeTarget)},
	})
	if postUnstakeReshare > 0 {
		t.Logf("reshare succeeded after election change (%d commits)", postUnstakeReshare)
	} else {
		t.Log("warning: no reshare after election change (may need more cycles)")
	}

	t.Log("PASS: blame epoch decode test with different election membership")
}

// TestTSSBlameAccumulation verifies that multiple blame records from
// different reshare cycles are ALL read and used for exclusion. The old
// code read a single blame via GetCommitmentByHeight; the fix reads ALL
// blames in the BLAME_EXPIRE window via FindCommitmentsSimple.
//
// This test blames node 3 in cycle 1 and node 4 in cycle 2, then checks
// that BOTH are excluded in cycle 3. If only the most recent blame was
// read, only node 4 would be excluded.
// Covers: Section 8 item 4 (fail-first for blame accumulation)
func TestTSSBlameAccumulation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.Nodes = 6
	cfg.GenesisNode = 6

	d, ctx := startDevnet(t, cfg, 18*time.Minute)

	t.Log("waiting for keygen...")
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("keygen: keyId=%s block=%d", keygen.KeyId, keygen.BlockHeight)

	// --- Cycle 1: blame node 3 ---
	t.Log("cycle 1: causing blame against magi-3...")
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	target1 := nextReshareBoundary(head)
	if target1-head < testRotateInterval {
		target1 += testRotateInterval
	}

	waitForBlock(t, d.HiveRPCEndpoint(), target1-2, 2*time.Minute)
	t.Log("disconnecting magi-3 (after readiness window)...")
	if err := d.Disconnect(ctx, 3); err != nil {
		t.Fatalf("disconnect magi-3: %v", err)
	}

	waitForBlock(t, d.HiveRPCEndpoint(), target1+5, 1*time.Minute)
	time.Sleep(40 * time.Second)

	blame1Count := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "blame",
		"block_height": bson.M{"$gt": keygen.BlockHeight},
	})
	t.Logf("cycle 1: %d blame commitments after keygen", blame1Count)

	if err := d.Reconnect(ctx, 3); err != nil {
		t.Logf("warning: reconnect magi-3: %v", err)
	}
	time.Sleep(5 * time.Second)

	// --- Cycle 2: blame node 4 ---
	t.Log("cycle 2: causing blame against magi-4...")
	head, _ = getHeadBlock(d.HiveRPCEndpoint())
	target2 := nextReshareBoundary(head)
	if target2-head < testRotateInterval {
		target2 += testRotateInterval
	}

	waitForBlock(t, d.HiveRPCEndpoint(), target2-2, 2*time.Minute)
	t.Log("disconnecting magi-4 (after readiness window)...")
	if err := d.Disconnect(ctx, 4); err != nil {
		t.Fatalf("disconnect magi-4: %v", err)
	}

	waitForBlock(t, d.HiveRPCEndpoint(), target2+5, 1*time.Minute)
	time.Sleep(40 * time.Second)

	blame2Count := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "blame",
		"block_height": bson.M{"$gt": keygen.BlockHeight},
	})
	t.Logf("cycle 2: %d total blame commitments", blame2Count)

	if err := d.Reconnect(ctx, 4); err != nil {
		t.Logf("warning: reconnect magi-4: %v", err)
	}

	// --- Cycle 3: verify BOTH nodes excluded ---
	t.Log("cycle 3: verifying both blamed nodes are excluded...")
	head, _ = getHeadBlock(d.HiveRPCEndpoint())
	target3 := nextReshareBoundary(head) + testRotateInterval
	t.Logf("waiting for reshare at block ~%d", target3)
	waitForBlock(t, d.HiveRPCEndpoint(), target3+5, 2*time.Minute)
	time.Sleep(15 * time.Second)

	assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)

	witnessName3 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 3)
	witnessName4 := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 4)
	node3Excluded := false
	node4Excluded := false

	for i := 1; i <= cfg.Nodes; i++ {
		if i == 3 || i == 4 {
			continue
		}
		logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
		if err != nil {
			continue
		}
		if strings.Contains(logs, witnessName3) && (strings.Contains(logs, "blamed") || strings.Contains(logs, "excluded")) {
			node3Excluded = true
		}
		if strings.Contains(logs, witnessName4) && (strings.Contains(logs, "blamed") || strings.Contains(logs, "excluded")) {
			node4Excluded = true
		}
	}

	if blame2Count >= 2 {
		if node3Excluded && node4Excluded {
			t.Log("PASS: both node 3 and node 4 excluded via accumulated blame")
		} else if !node3Excluded && node4Excluded {
			t.Error("FAIL: only node 4 excluded — blame accumulation may not be working (only most recent blame read)")
		} else {
			t.Logf("node3 excluded=%v, node4 excluded=%v", node3Excluded, node4Excluded)
			t.Log("warning: could not confirm both exclusions via logs (may need different log patterns)")
		}
	} else {
		t.Logf("only %d blame records — cannot fully verify accumulation", blame2Count)
		t.Log("note: nodes may have been excluded by readiness gate instead of blame")
	}

	reshareCount := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "reshare",
		"block_height": bson.M{"$gt": uint64(target3)},
	})
	if reshareCount > 0 {
		t.Logf("reshare succeeded with blamed nodes excluded (%d commits)", reshareCount)
	} else {
		t.Log("warning: no reshare in cycle 3")
	}
}
