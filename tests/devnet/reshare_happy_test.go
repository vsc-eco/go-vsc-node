package devnet

import (
	"testing"
	"time"
)

// TestTSSReshareHappyPath verifies the complete happy path: keygen
// completes, on-chain readiness is broadcast, and a reshare commitment
// lands on Hive with all nodes healthy. This is the baseline smoke test
// for the on-chain gossip architecture.
// Covers: Section 8 item 2 (full Hive broadcast path tested)
func TestTSSReshareHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnet(t, cfg, 15*time.Minute)

	// Step 1: Keygen must complete after genesis election
	t.Log("waiting for keygen...")
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("keygen: keyId=%s epoch=%d block=%d", keygen.KeyId, keygen.Epoch, keygen.BlockHeight)

	// Step 2: Wait for reshare trigger
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	target := nextReshareBoundary(head)
	t.Logf("head=%d, waiting for reshare at block %d", head, target)
	waitForBlock(t, d.HiveRPCEndpoint(), target+5, 2*time.Minute)

	// Step 3: Reshare commitment must land
	t.Log("waiting for reshare commitment...")
	reshare := waitForCommitment(t, d.MongoURI(), "reshare", 2*time.Minute)
	t.Logf("reshare: keyId=%s epoch=%d block=%d", reshare.KeyId, reshare.Epoch, reshare.BlockHeight)

	// Assertions
	assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)

	if node, ok := anyNodeLogsContain(d, ctx, cfg.Nodes, "signed readiness attestation"); ok {
		t.Logf("magi-%d confirmed gossip readiness attestation", node)
	} else {
		t.Error("no node logged readiness attestation")
	}

	if node, ok := anyNodeLogsContain(d, ctx, cfg.Nodes, "reshare pre-flight checks passed"); ok {
		t.Logf("magi-%d confirmed pre-flight passed", node)
	} else {
		t.Error("no node logged pre-flight passed")
	}
}
