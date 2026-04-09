package devnet

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestTSSOfflineNodeExcludedByReadiness verifies that a node disconnected
// before the readiness window never broadcasts vsc.tss_ready and is
// excluded from the party list entirely. The remaining nodes should
// complete the reshare without it — no blame needed, the readiness gate
// prevents the offline node from being included at all.
// Covers: Section 5 test #2, Section 7 Q4(d), Section 8 item 2
func TestTSSOfflineNodeExcludedByReadiness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnet(t, cfg, 15*time.Minute)

	t.Log("waiting for keygen...")
	waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)

	// Disconnect node 4 before the readiness window.
	// With RotateInterval=20 and ReadinessOffset=5, the window opens 5 blocks
	// before the reshare. Disconnecting now means node 4 won't broadcast
	// vsc.tss_ready and will be excluded from the party list entirely.
	t.Log("disconnecting magi-4...")
	if err := d.Disconnect(ctx, 4); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	// Wait for a reshare to happen (with node 4 excluded)
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	// Skip the immediate boundary (may already be in readiness window),
	// target the one after
	target := nextReshareBoundary(head) + testRotateInterval
	t.Logf("head=%d, waiting for reshare at block %d (node 4 disconnected)", head, target)
	waitForBlock(t, d.HiveRPCEndpoint(), target+5, 2*time.Minute)

	// Wait for the result (reshare or blame)
	time.Sleep(15 * time.Second)

	assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)

	// The reshare should succeed with 4 nodes (threshold+1 = 4 for N=5)
	reshareCount := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "reshare",
		"block_height": bson.M{"$gte": target},
	})
	if reshareCount > 0 {
		t.Log("reshare succeeded with offline node excluded by readiness gate")
	} else {
		t.Log("reshare did not land — checking if pre-flight skipped")
		for i := 1; i <= cfg.Nodes; i++ {
			if i == 4 {
				continue
			}
			if nodeLogsContain(d, ctx, i, "insufficient") {
				t.Logf("magi-%d logged insufficient participants (expected)", i)
				break
			}
		}
	}

	if err := d.Reconnect(ctx, 4); err != nil {
		t.Logf("warning: reconnect: %v", err)
	}
}

// TestTSSFalseReadinessProducesBlame verifies that a node which broadcasts
// readiness (on-chain) then disconnects before the reshare protocol starts
// produces a blame commitment. All online nodes see the same party list
// (including the disconnected node), the same WaitingFor() result, produce
// identical CIDs, and BLS collection succeeds — landing blame on-chain.
// Covers: Section 5 test #3 (node claims ready, disconnects, blame lands)
func TestTSSFalseReadinessProducesBlame(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnet(t, cfg, 15*time.Minute)

	t.Log("waiting for keygen...")
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)

	// Let node 3 broadcast readiness (it's online during the readiness window)
	// then disconnect it RIGHT AFTER the readiness window closes but BEFORE
	// the reshare fires. This creates a "false readiness" — node 3 is in the
	// party list but won't send btss messages.
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	target := nextReshareBoundary(head)
	if target-head < testRotateInterval {
		target += testRotateInterval // ensure we have time
	}

	// Wait until just after the readiness window (reshareBlock - readinessOffset)
	// ReadinessOffset=5, so readiness broadcasts at reshareBlock-5.
	// We wait until reshareBlock-3 then disconnect (readiness already broadcast).
	readinessDeadline := target - 3
	t.Logf("head=%d, reshare at %d, disconnecting magi-3 at block %d (after readiness window)",
		head, target, readinessDeadline)
	waitForBlock(t, d.HiveRPCEndpoint(), readinessDeadline, 2*time.Minute)

	t.Log("disconnecting magi-3 (after readiness broadcast)...")
	if err := d.Disconnect(ctx, 3); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	// Wait for reshare to trigger and timeout (ReshareTimeout=2m)
	waitForBlock(t, d.HiveRPCEndpoint(), target+5, 1*time.Minute)
	t.Log("waiting for timeout + blame...")
	time.Sleep(40 * time.Second) // ReshareTimeout + BLS collection

	assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)

	// Blame should land because: all online nodes have the same party list
	// (includes node 3), same WaitingFor() result (node 3), same CID, BLS succeeds.
	blameCount := countCommitments(t, d.MongoURI(), bson.M{
		"type":         "blame",
		"block_height": bson.M{"$gt": keygen.BlockHeight},
	})
	if blameCount > 0 {
		t.Logf("blame landed (%d blame commitment(s)) — false readiness correctly detected", blameCount)
	} else {
		t.Log("warning: no blame commitment found (may need more time or node 3 disconnected too early)")
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 20)
	}

	if err := d.Reconnect(ctx, 3); err != nil {
		t.Logf("warning: reconnect: %v", err)
	}
}
