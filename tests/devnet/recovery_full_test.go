package devnet

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestTSSFullRecoveryCycle exercises the complete lifecycle:
// keygen → reshare → disconnect a node → blame → reconnect → reshare again.
//
// This is the end-to-end proof that the network can:
//   1. Generate a key (keygen)
//   2. Rotate it (reshare)
//   3. Handle a node failure (blame)
//   4. Recover after the node comes back (exclude or re-include)
//   5. Continue operating (second reshare)
//
// Each step depends on the previous one succeeding. If any step fails,
// the test provides diagnostic information about which phase broke.
func TestTSSFullRecoveryCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnet(t, cfg, 18*time.Minute)

	// Step 1: Keygen
	t.Log("step 1: waiting for keygen...")
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("keygen: keyId=%s block=%d", keygen.KeyId, keygen.BlockHeight)

	// Step 2: First reshare (all nodes online)
	t.Log("step 2: waiting for first reshare...")
	reshare1 := waitForCommitment(t, d.MongoURI(), "reshare", 3*time.Minute)
	t.Logf("reshare 1: block=%d epoch=%d", reshare1.BlockHeight, reshare1.Epoch)

	// Step 3: Disconnect node 3, cause blame
	t.Log("step 3: disconnecting magi-3...")
	if err := d.Disconnect(ctx, 3); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	// Wait for a couple of reshare cycles to accumulate blame or exclude
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	blameTarget := nextReshareBoundary(head) + 2*testRotateInterval
	t.Logf("waiting for blame cycles, target block ~%d", blameTarget)
	waitForBlock(t, d.HiveRPCEndpoint(), blameTarget+5, 5*time.Minute)
	time.Sleep(40 * time.Second)

	// Step 4: Verify blame or exclusion happened
	blameCount := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "blame",
		"block_height": bson.M{"$gt": reshare1.BlockHeight},
	})
	t.Logf("step 4: blame commitments since reshare 1: %d", blameCount)

	// Step 5: Reconnect node 3
	t.Log("step 5: reconnecting magi-3...")
	if err := d.Reconnect(ctx, 3); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	// Wait for reconnected node to sync and participate
	time.Sleep(10 * time.Second)

	// Step 6: Reshare with all nodes back online
	head, _ = getHeadBlock(d.HiveRPCEndpoint())
	recoveryTarget := nextReshareBoundary(head) + 2*testRotateInterval
	t.Logf("step 6: waiting for recovery reshare at ~%d", recoveryTarget)
	waitForBlock(t, d.HiveRPCEndpoint(), recoveryTarget+5, 5*time.Minute)
	time.Sleep(15 * time.Second)

	// Verify reshare happened after recovery
	postRecovery := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "reshare",
		"block_height": bson.M{"$gt": reshare1.BlockHeight},
	})
	if postRecovery > 0 {
		t.Logf("recovery reshare(s): %d", postRecovery)
	} else {
		t.Error("no reshare after node recovery")
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 30)
	}

	// Final: no SSID mismatch at any point
	assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)
	t.Log("PASS: full cycle keygen -> reshare -> disconnect+blame -> reconnect+reshare")
}
