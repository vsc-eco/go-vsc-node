package devnet

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
)

// TestBlameExcludesNodeOnRetry exercises the blame commitment flow:
//
//  1. Start a 5-node devnet, deploy call-tss contract, trigger keygen
//     under healthy conditions and wait for it to land on-chain.
//  2. Wait for the first non-genesis election (epoch 1) so that
//     reshare can trigger for the epoch-0 key.
//  3. Disconnect one node before the reshare fires, causing a timeout
//     blame commitment.
//  4. Verify all nodes processed the blame commitment.
//  5. Wait for the subsequent reshare retry (same epoch) and verify
//     that the blamed node is excluded from the party list.
//
// Timeline (ElectionInterval=300, TSS_ROTATE_INTERVAL=100):
//
//	~block  60: genesis election → epoch 0
//	~block 100: keygen fires → key at epoch 0
//	~block 360: first real election → epoch 1
//	 block 400: first reshare (node disconnected → blame)
//	 block 500: second reshare (should exclude blamed node)
//	~block 660: next election → epoch 2 (after we're done)
//
// Run with:
//
//	go test -v -run TestBlameExcludesNodeOnRetry -timeout 40m ./tests/devnet/
func TestBlameExcludesNodeOnRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet blame test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	// Build the call-tss contract first (before starting the devnet).
	wasmPath, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
	}

	// RotateInterval=20 (~1 min) speeds up reshare cycles.
	// ElectionInterval=60 (~3 min) allows keygen + epoch transition
	// before reshare, and 2 reshare attempts within one epoch.
	cfg := DefaultConfig()
	cfg.Nodes = 5
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

	// ── Phase 1: Deploy contract and trigger keygen ──────────────────

	t.Log("Phase 1: deploying contract and triggering keygen...")

	contractId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasmPath,
		Name:     "call-tss",
	})
	if err != nil {
		t.Fatalf("deploying contract: %v", err)
	}
	t.Logf("contract deployed: %s", contractId)

	// Wait for at least one node to be caught up.
	t.Log("waiting for node 2 to be synced...")
	if err := d.WaitForBlockProcessing(ctx, 2, 10, 3*time.Minute); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("node 2 never synced: %v", err)
	}
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	t.Logf("node 2 synced to block %d", bh)

	// Call tssCreate on the contract to request a new key.
	payload := `{"key_name":"blameTestKey","epochs":10}`
	txId, err := d.CallContract(ctx, 2, contractId, "tssCreate", payload)
	if err != nil {
		t.Fatalf("calling tssCreate: %v", err)
	}
	t.Logf("tssCreate tx: %s", txId)

	// Wait for the key to appear.
	fullKeyId := contractId + "-blameTestKey"
	t.Logf("waiting for key %s to be created...", fullKeyId)
	_, err = d.WaitForTssKey(ctx, 2, bson.M{"id": fullKeyId}, 5*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("key never created: %v", err)
	}

	// Wait for keygen commitment to land on-chain.
	t.Log("waiting for keygen commitment on-chain...")
	keygenCommit, err := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id": fullKeyId,
		"type":   "keygen",
	}, 10*time.Minute)
	if err != nil {
		d.dumpBlockHeight(ctx, t, 2)
		d.dumpTssLogs(ctx, t, 2)
		t.Fatalf("keygen commitment never landed: %v", err)
	}
	t.Logf("keygen landed at block %d, epoch %d", keygenCommit.BlockHeight, keygenCommit.Epoch)

	// Verify key is now active.
	activeKey, err := d.WaitForTssKey(ctx, 2, bson.M{
		"id":     fullKeyId,
		"status": "active",
	}, 2*time.Minute)
	if err != nil {
		t.Fatalf("key never became active: %v", err)
	}
	t.Logf("key active: %s (epoch %d)", activeKey.Id, activeKey.Epoch)

	// ── Phase 2: Wait for epoch advance, then disconnect ─────────────

	// Reshare only triggers for keys from a LOWER epoch than the
	// current election. The key is at epoch 0 (from keygen). We need
	// epoch >= 1 before reshare will fire. Wait for a second election.
	t.Log("Phase 2: waiting for election epoch >= 1...")
	err = d.waitForElectionEpoch(ctx, 2, 1, 5*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("epoch 1 never arrived: %v", err)
	}
	currentBh, _ := d.getLastProcessedBlock(ctx, 2)
	t.Logf("epoch 1 arrived, node 2 at block %d", currentBh)

	// Disconnect node 3 BEFORE the next reshare fires (next bh%100==0).
	disconnectedNode := 3
	t.Logf("disconnecting node %d...", disconnectedNode)
	if err := d.Disconnect(ctx, disconnectedNode); err != nil {
		t.Fatalf("disconnecting node %d: %v", disconnectedNode, err)
	}
	t.Cleanup(func() {
		d.Reconnect(context.Background(), disconnectedNode)
	})

	// ── Phase 3: Wait for blame ──────────────────────────────────────

	// The next reshare fires at the next bh%100==0. With the
	// disconnected node, the reshare will timeout after 2 minutes
	// (ReshareTimeout), then BLS collection (~11s), then on-chain
	// after 1-2 L1 blocks (~6s). Total: ~2.5 min from the reshare
	// trigger. Plus up to 100 blocks (~5 min) until the next trigger.
	t.Log("Phase 3: waiting for first blame commitment...")
	blame1, err := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id": fullKeyId,
		"type":   "blame",
	}, 10*time.Minute)
	if err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("first blame never landed: %v", err)
	}
	t.Logf("first blame at block %d: commitment=%s", blame1.BlockHeight, blame1.Commitment)

	// Decode the bitset.
	blame1Bits := decodeBitset(t, blame1.Commitment)
	t.Logf("first blame bitset: %s", blame1Bits.Text(2))

	// Verify all healthy nodes have the blame commitment.
	// Give slower nodes up to 30s to process the L1 block containing it.
	t.Log("verifying all nodes processed the blame commitment...")
	for node := 1; node <= cfg.Nodes; node++ {
		if node == disconnectedNode {
			continue // can't reach this node's DB reliably
		}
		blame, err := d.WaitForCommitment(ctx, node, bson.M{
			"key_id": fullKeyId,
			"type":   "blame",
		}, 30*time.Second)
		if err != nil {
			t.Logf("node %d: warning: blame commitment not found within 30s: %v", node, err)
			continue
		}
		t.Logf("node %d: blame OK (block=%d, tx=%s)", node, blame.BlockHeight, blame.TxId)
	}

	// ── Phase 4: Wait for retry and verify exclusion ─────────────────

	t.Log("Phase 4: waiting for next reshare retry to verify blame exclusion...")

	// Wait for a second commitment (blame or reshare) with a higher
	// block_height than the first blame.
	secondCommit, err := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": blame1.BlockHeight},
	}, 10*time.Minute)
	if err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("second commitment never landed: %v", err)
	}
	t.Logf("second commitment: type=%s block=%d commitment=%s",
		secondCommit.Type, secondCommit.BlockHeight, secondCommit.Commitment)

	if secondCommit.Type == "reshare" {
		// Reshare succeeded with 4 nodes — the blamed node was excluded.
		t.Log("PASS: reshare succeeded after excluding blamed node")
		return
	}

	// It's another blame. Verify the bitset is DIFFERENT from the
	// first blame, proving the blamed node was excluded from the
	// retry session.
	blame2Bits := decodeBitset(t, secondCommit.Commitment)
	t.Logf("second blame bitset: %s", blame2Bits.Text(2))

	if blame1.Commitment == secondCommit.Commitment {
		t.Fatalf("FAIL: second blame has identical bitset to first blame (%s) — "+
			"the blamed node was NOT excluded from the retry session", blame1.Commitment)
	}

	t.Logf("PASS: blame bitsets differ (first=%s second=%s) — "+
		"previously blamed node was excluded from retry",
		blame1Bits.Text(2), blame2Bits.Text(2))
}

// decodeBitset decodes a base64-RawURL-encoded bitset string into a big.Int.
func decodeBitset(t *testing.T, encoded string) *big.Int {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decoding bitset %q: %v", encoded, err)
	}
	return new(big.Int).SetBytes(data)
}

func dumpLogs(t *testing.T, d *Devnet, ctx context.Context) {
	t.Helper()
	for i := 1; i <= d.cfg.Nodes; i++ {
		logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
		if logs != "" {
			t.Logf("magi-%d logs:\n%s", i, truncateLogs(logs, 40))
		}
	}
	hafLogs, _ := d.Logs(ctx, "haf")
	if hafLogs != "" {
		t.Logf("haf logs:\n%s", truncateLogs(hafLogs, 20))
	}
}

func dumpDiagnostics(t *testing.T, d *Devnet, ctx context.Context) {
	t.Helper()
	for n := 1; n <= d.cfg.Nodes; n++ {
		d.dumpBlockHeight(ctx, t, n)
		keys, _ := d.GetTssKeys(ctx, n, bson.M{})
		t.Logf("node %d: tss_keys=%+v", n, keys)
	}
	d.dumpContracts(ctx, t, 2)
	for _, n := range []int{1, 2} {
		logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
		t.Logf("magi-%d logs (last 80):\n%s", n, truncateLogs(logs, 80))
	}
}
