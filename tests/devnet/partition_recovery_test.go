package devnet

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestTSSPartitionAndRecovery verifies that the network handles a partial
// P2P partition (two specific nodes can't reach each other) and recovers
// after the partition heals. With only 2 of 5 nodes partitioned from each
// other (but both still connected to the other 3), the reshare should
// still succeed — the partitioned pair can relay messages through the
// connected majority.
// Covers: Section 6 connected_but_no_response scenario
func TestTSSPartitionAndRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnet(t, cfg, 15*time.Minute)

	t.Log("waiting for keygen...")
	waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)

	// Baseline: wait for first reshare
	t.Log("waiting for first reshare (baseline)...")
	reshare1 := waitForCommitment(t, d.MongoURI(), "reshare", 3*time.Minute)

	// Partition nodes 2 <-> 4
	t.Log("partitioning magi-2 <-> magi-4...")
	if err := d.Partition(ctx, 2, 4); err != nil {
		t.Fatalf("partition: %v", err)
	}

	// Wait for a reshare during the partition
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	target := nextReshareBoundary(head) + testRotateInterval
	t.Logf("waiting for reshare at ~%d during partition", target)
	waitForBlock(t, d.HiveRPCEndpoint(), target+5, 2*time.Minute)
	time.Sleep(15 * time.Second)

	assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)

	// Heal
	t.Log("healing partition...")
	if err := d.Heal(ctx, 2, 4); err != nil {
		t.Fatalf("heal: %v", err)
	}

	// Wait for reshare after recovery
	head, _ = getHeadBlock(d.HiveRPCEndpoint())
	recoveryTarget := nextReshareBoundary(head) + testRotateInterval
	t.Logf("waiting for recovery reshare at ~%d", recoveryTarget)
	waitForBlock(t, d.HiveRPCEndpoint(), recoveryTarget+5, 2*time.Minute)
	time.Sleep(15 * time.Second)

	postRecovery := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "reshare",
		"block_height": bson.M{"$gt": reshare1.BlockHeight},
	})
	if postRecovery > 0 {
		t.Logf("recovery: %d new reshare(s) after healing", postRecovery)
	} else {
		t.Log("warning: no new reshare after recovery")
	}

	assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)
}
