package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// TestOraclePrunedBlockFetch verifies the oracle's fetch-from-peer recovery
// path. When the local bitcoind has pruned old block data, the oracle should
// detect the pruned-block RPC error, request the missing blocks from an
// archive peer via getblockfrompeer, and successfully relay them.
//
// Setup:
//   - Two bitcoind containers: archive (full data, txindex) and pruned
//     (manual pruning via -prune=1), connected via P2P.
//   - Large OP_RETURN transactions are created on the archive node to fill
//     the 128 MB block file threshold required for Bitcoin Core to allow
//     pruning (blk00000.dat must be full before it can be pruned).
//   - Blocks propagate to the pruned node, then pruneblockchain removes
//     old block data from the first file.
//   - Magi nodes are configured to use the pruned bitcoind as their RPC
//     source, with the contract initialized at a height in the pruned range.
//
// Assertions:
//   - The oracle logs "pruned blocks recovered successfully" (fetch-from-peer
//     worked).
//   - A chain relay transaction is successfully submitted (end-to-end success).
//
// This test takes ~10-15 minutes due to block data generation.
//
// Run with:
//
//	go test -v -run TestOraclePrunedBlockFetch -timeout 45m ./tests/devnet/
func TestOraclePrunedBlockFetch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet oracle pruned-fetch test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	// ── Build the stub BTC mapping contract ─────────────────────────────

	stubWasm, err := BuildBtcStubContract(ctx)
	if err != nil {
		t.Fatalf("building btc-stub contract: %v", err)
	}

	// ── Devnet config ───────────────────────────────────────────────────

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.LogLevel = "info,oracle=debug,chain-relay=debug,oracle-btc=debug"
	cfg.EnableBitcoind = true // starts the archive bitcoind

	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 60,
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

	// ── Fill block data to enable pruning ────────────────────────────────
	//
	// Bitcoin Core stores blocks in 128 MB files (blk?????.dat) and can
	// only prune complete files. With tiny regtest blocks (~1 KB each),
	// we'd need ~130,000 blocks to fill one file. Instead, we create
	// ~200 KB OP_RETURN transactions to fill blk00000.dat in ~700 blocks.

	t.Log("filling block data on archive bitcoind (target: 128 MB)...")
	if err := d.FillBlockData(ctx); err != nil {
		t.Fatalf("filling block data: %v", err)
	}

	archiveTip, err := d.BitcoinHeight(ctx)
	if err != nil {
		t.Fatalf("BitcoinHeight: %v", err)
	}
	t.Logf("archive bitcoind tip after fill: %d", archiveTip)

	// ── Start the pruned bitcoind and wait for sync ─────────────────────

	t.Log("starting bitcoind-pruned (manual pruning, peer of archive)...")
	if err := d.StartBitcoindPruned(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting bitcoind-pruned: %v", err)
	}

	t.Log("waiting for pruned node to sync...")
	if err := d.WaitForBitcoinPrunedSync(ctx, archiveTip, 5*time.Minute); err != nil {
		t.Fatalf("pruned node sync: %v", err)
	}
	prunedTip, _ := d.BitcoinPrunedHeight(ctx)
	t.Logf("pruned bitcoind tip: %d (synced)", prunedTip)

	// ── Prune old blocks ────────────────────────────────────────────────
	//
	// With blk00000.dat full and blk00001.dat started, pruneblockchain
	// can now delete blk00000.dat, making all blocks in that file
	// unavailable via getblock (returns "Block not available (pruned data)").

	// Prune up to a height well into blk00000.dat's range.
	pruneHeight := archiveTip / 2
	lastPruned, err := d.PruneBlockchain(ctx, pruneHeight)
	if err != nil {
		t.Fatalf("pruneblockchain %d: %v", pruneHeight, err)
	}
	t.Logf("pruned blocks up to height %d (bitcoind reports last pruned: %d)", pruneHeight, lastPruned)

	// Verify pruning worked by trying to fetch a block in the pruned range.
	verifyHeight := lastPruned / 2
	if verifyHeight < 1 {
		verifyHeight = 1
	}
	blockHash, err := d.bitcoinCliPruned(ctx, "getblockhash", fmt.Sprint(verifyHeight))
	if err != nil {
		t.Fatalf("getblockhash %d failed on pruned node: %v", verifyHeight, err)
	}
	verifyOut, err := d.bitcoinCliPruned(ctx, "getblock", blockHash)
	if err != nil {
		t.Logf("confirmed: block at height %d is pruned (getblock failed as expected)", verifyHeight)
	} else {
		t.Fatalf("expected pruned block error at height %d but got data: %s", verifyHeight, verifyOut[:min(len(verifyOut), 100)])
	}

	// ── Deploy stub contract and wire oracle configs ─────────────────────

	t.Log("deploying btc-stub mapping contract...")
	contractId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     stubWasm,
		Name:         "btc-stub",
		DeployerNode: 1,
		GQLNode:      2,
	})
	if err != nil {
		t.Fatalf("deploying btc-stub: %v", err)
	}
	t.Logf("btc-stub deployed: %s", contractId)

	// Point all magi nodes at the PRUNED bitcoind (not the archive).
	if err := d.WriteOracleConfigsForHost(ctx, d.BitcoindPrunedRPCHostPort()); err != nil {
		t.Fatalf("writing oracle configs: %v", err)
	}
	if err := d.SetOracleContractIDs(map[string]string{"BTC": contractId}); err != nil {
		t.Fatalf("setting ChainContracts: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("restarting magi nodes: %v", err)
	}

	// ── Initialize the contract in the pruned range ─────────────────────
	//
	// Set start_height inside the pruned range. The oracle will try to
	// fetch blocks starting from start_height+1, hitting the pruned-data
	// error and triggering the fetch-from-peer recovery path.

	startHeight := lastPruned / 4 // well within the pruned range
	if startHeight < 1 {
		startHeight = 1
	}
	initPayload := fmt.Sprintf(`{"start_height":%d}`, startHeight)
	t.Logf("initializing contract at start_height=%d (inside pruned range, last pruned=%d)", startHeight, lastPruned)
	if _, err := d.CallContract(ctx, 2, contractId, "init", initPayload); err != nil {
		t.Fatalf("init contract: %v", err)
	}

	// Wait for the init transaction to land.
	t.Log("waiting for init transaction to land...")
	initLanded := waitForLogAnyNode(t, d, ctx,
		"initiating chain relay consensus",
		3*time.Minute)
	if initLanded == 0 {
		initLanded = waitForLogAnyNode(t, d, ctx,
			"no new blocks to relay",
			30*time.Second)
	}
	if initLanded == 0 {
		dumpOracleDiagnostics(t, d, ctx)
		t.Fatalf("init transaction never landed (test setup is broken)")
	}
	t.Logf("init landed (magi-%d read contract state successfully)", initLanded)

	// ── Wait for pruned-block recovery ──────────────────────────────────
	//
	// The oracle should:
	//   1. Try to fetch blocks from the pruned node
	//   2. Get pruned-block RPC errors
	//   3. Call getblockfrompeer to recover from the archive peer
	//   4. Log "pruned blocks recovered successfully"
	//   5. Submit the relay transaction

	t.Log("waiting for pruned-block recovery and relay submission...")
	recoveredOn := waitForLogAnyNode(t, d, ctx,
		"pruned blocks recovered successfully",
		4*time.Minute)

	submittedOn := waitForLogAnyNode(t, d, ctx,
		"chain relay transaction submitted",
		2*time.Minute)

	dumpOracleDiagnostics(t, d, ctx)

	// ── Assertions ──────────────────────────────────────────────────────

	if recoveredOn == 0 {
		t.Errorf("FAIL: no node logged 'pruned blocks recovered successfully' — fetch-from-peer did not trigger or did not succeed")
	} else {
		t.Logf("PASS: magi-%d recovered pruned blocks from archive peer", recoveredOn)
	}

	if submittedOn == 0 {
		t.Errorf("FAIL: no successful chain relay submission after pruned-block recovery")
	} else {
		t.Logf("PASS: magi-%d submitted chain relay transaction after pruned-block recovery", submittedOn)
	}

	// Verify no "giving up" or "no archive peers" warnings.
	gaveUp := countLogOccurrencesAllNodes(d, ctx, "giving up on pruned block recovery")
	noPeers := countLogOccurrencesAllNodes(d, ctx, "no archive (NODE_NETWORK) peers connected")
	if gaveUp > 0 {
		t.Errorf("WARN: saw %d 'giving up on pruned block recovery' log lines — some fetch attempts failed", gaveUp)
	}
	if noPeers > 0 {
		t.Errorf("WARN: saw %d 'no archive peers' log lines — peer filtering may be too aggressive", noPeers)
	}
}
