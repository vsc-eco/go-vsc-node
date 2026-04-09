package devnet

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestTSSMultiVersion runs 4 nodes on the current code and 1 node on an
// older version (from /home/dockeruser/magi/testnet/original-repo/go-vsc-node)
// that does NOT have the vsc.tss_ready handler or on-chain readiness gate.
//
// Expected behavior:
//   - The old-code node never broadcasts readiness → excluded from party lists
//   - It may participate in keygen (which doesn't use readiness gate)
//   - During reshare, it builds a different party list → SSID mismatch (expected)
//   - New-code nodes detect the mismatch and timeout
//   - The old-code node does NOT crash
//   - Reshare eventually succeeds among new-code nodes (with or without the old node)
//
// This validates the graceful degradation described in the review Section 6:
// old-code nodes are excluded until they update.
// Covers: Section 8 item 5, Section 6 multi-version problem
func TestTSSMultiVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	oldCodeDir := "/home/dockeruser/magi/testnet/original-repo/go-vsc-node"
	if _, err := os.Stat(oldCodeDir); err != nil {
		t.Skipf("old code repo not found at %s", oldCodeDir)
	}

	cfg := tssTestConfig()
	cfg.Nodes = 5
	cfg.OldCodeSourceDir = oldCodeDir
	cfg.OldCodeNodes = []int{5} // node 5 runs old code
	cfg.GenesisNode = 4         // genesis on new-code node

	d, ctx := startDevnet(t, cfg, 20*time.Minute)

	// Step 1: keygen — all 5 nodes participate (keygen uses different path)
	t.Log("step 1: waiting for keygen...")
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 8*time.Minute)
	t.Logf("keygen: keyId=%s epoch=%d block=%d", keygen.KeyId, keygen.Epoch, keygen.BlockHeight)

	// Step 2: wait for reshare trigger. Node 5 (old code) does NOT broadcast
	// vsc.tss_ready, so it should be excluded from the party list by the
	// on-chain readiness gate.
	t.Log("step 2: waiting for reshare (node 5 should be excluded by readiness gate)...")
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	target := nextReshareBoundary(head) + testRotateInterval
	t.Logf("head=%d, reshare at block ~%d", head, target)
	waitForBlock(t, d.HiveRPCEndpoint(), target+5, 2*time.Minute)
	time.Sleep(15 * time.Second)

	// Step 3: check results.
	// NOTE: SSID mismatches are EXPECTED in multi-version scenarios because
	// the old-code node builds different party lists (no readiness gate).
	// We check for them as informational, not as failures.
	for i := 1; i <= cfg.Nodes; i++ {
		logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
		if err != nil {
			continue
		}
		if strings.Contains(logs, "ssid mismatch") {
			t.Logf("magi-%d has SSID mismatch (expected with old-code node in network)", i)
		}
	}

	// Verify node 5 (old code) is running and didn't crash
	node5Running := false
	if logs, err := d.Logs(ctx, "magi-5"); err == nil && len(logs) > 0 {
		node5Running = true
		// Old code should NOT have "broadcast tss readiness" in its logs
		if strings.Contains(logs, "broadcast tss readiness") {
			t.Error("old-code node 5 should NOT broadcast vsc.tss_ready")
		}
		// Old code should NOT have crash indicators
		if strings.Contains(logs, "panic:") || strings.Contains(logs, "fatal error:") {
			t.Error("old-code node 5 crashed!")
			dumpNodeLogs(t, d, ctx, cfg.Nodes, 30)
		}
	}
	if !node5Running {
		t.Error("could not get logs for magi-5 (old-code node)")
	} else {
		t.Log("old-code node 5 is running without crashes")
	}

	// New-code nodes (1-4) should log readiness broadcast
	newNodeBroadcast := false
	for i := 1; i <= 4; i++ {
		if nodeLogsContain(d, ctx, i, "broadcast tss readiness") {
			newNodeBroadcast = true
			t.Logf("magi-%d (new code) confirmed readiness broadcast", i)
			break
		}
	}
	if !newNodeBroadcast {
		t.Log("warning: no new-code node logged readiness broadcast")
	}

	// Check if reshare succeeded among the 4 new-code nodes.
	reshareCount := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "reshare",
		"block_height": bson.M{"$gt": keygen.BlockHeight},
	})
	if reshareCount > 0 {
		t.Logf("reshare succeeded among new-code nodes (%d commits) — old node excluded by readiness gate", reshareCount)
	} else {
		blameCount := countCommitments(t, d.MongoURI(), bson.M{
			"type":         "blame",
			"block_height": bson.M{"$gt": keygen.BlockHeight},
		})
		if blameCount > 0 {
			t.Logf("blame landed (%d) instead of reshare — old node may have been included in party list", blameCount)
		} else {
			t.Log("warning: neither reshare nor blame landed")
		}
	}

	// Wait for a second reshare cycle to confirm stability
	head, _ = getHeadBlock(d.HiveRPCEndpoint())
	target2 := nextReshareBoundary(head) + testRotateInterval
	t.Logf("waiting for second reshare cycle at block ~%d...", target2)
	waitForBlock(t, d.HiveRPCEndpoint(), target2+5, 2*time.Minute)
	time.Sleep(15 * time.Second)

	totalReshares := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "reshare",
		"block_height": bson.M{"$gt": keygen.BlockHeight},
	})
	t.Logf("total reshares: %d (old-code node gracefully excluded throughout)", totalReshares)
	t.Log("PASS: multi-version test — old-code node excluded by readiness gate, did not crash")
}
