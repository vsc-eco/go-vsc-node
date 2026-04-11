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

// TestOraclePrunedBlockFetchMainnet verifies the oracle's fetch-from-peer
// recovery path using a real mainnet bitcoind with auto-pruning.
//
// Unlike TestOraclePrunedBlockFetch (which creates artificial block data on
// regtest), this test runs a mainnet bitcoind with -prune=550 and lets it
// sync from real Bitcoin peers. Mainnet blocks are large enough (~1-2 MB)
// that auto-pruning kicks in within a few minutes of IBD, naturally
// creating the pruned-block scenario without any artificial setup.
//
// The connected mainnet peers serve as archive (NODE_NETWORK) sources for
// getblockfrompeer recovery — no second bitcoind container needed.
//
// Requirements:
//   - Internet access (connects to Bitcoin mainnet peers via DNS seeds)
//   - ~5-15 minutes for IBD to reach the pruning threshold
//
// Run with:
//
//	go test -v -run TestOraclePrunedBlockFetchMainnet -timeout 45m ./tests/devnet/
func TestOraclePrunedBlockFetchMainnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet oracle mainnet pruned-fetch test in short mode")
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
	// No regtest bitcoind needed — we use a mainnet node instead.

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.LogLevel = "info,oracle=debug,chain-relay=debug,oracle-btc=debug"
	cfg.EnableBitcoind = false // no regtest bitcoind

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

	// ── Start mainnet-pruned bitcoind ────────────────────────────────────
	//
	// Runs bitcoind on mainnet with -prune=550 (auto-prune at 550 MB).
	// It connects to real Bitcoin peers via DNS seeds and starts IBD.
	// As it syncs, early blocks fill past 550 MB and get auto-pruned.

	t.Log("starting bitcoind-mainnet-pruned (mainnet, auto-prune at 550 MB)...")
	if err := d.StartBitcoindMainnetPruned(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting bitcoind-mainnet-pruned: %v", err)
	}

	// ── Wait for auto-pruning to kick in ────────────────────────────────
	//
	// Mainnet blocks are ~1-2 MB each. At ~500 blocks/minute during IBD,
	// it takes ~5-10 minutes to download 550 MB and trigger the first
	// prune. We poll getblockchaininfo for pruneheight > 0.

	t.Log("waiting for mainnet IBD to trigger auto-pruning (550 MB threshold)...")
	pruneHeight, err := d.WaitForMainnetPruning(ctx, 20*time.Minute)
	if err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("waiting for pruning: %v", err)
	}
	t.Logf("auto-pruning active: blocks below height %d are pruned", pruneHeight)

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

	// Point all magi nodes at the mainnet-pruned bitcoind.
	if err := d.WriteOracleConfigsForHost(ctx, d.BitcoindMainnetPrunedRPCHostPort()); err != nil {
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
	// Set start_height well below pruneheight so the oracle must fetch
	// pruned blocks. The oracle will try blocks starting at start_height+1,
	// which are all in the pruned range.

	startHeight := pruneHeight / 2
	if startHeight < 1 {
		startHeight = 1
	}
	initPayload := fmt.Sprintf(`{"start_height":%d}`, startHeight)
	t.Logf("initializing contract at start_height=%d (pruned range, pruneheight=%d)", startHeight, pruneHeight)
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
	//   1. Try to fetch blocks from the pruned mainnet node
	//   2. Get pruned-block RPC errors
	//   3. Call getblockfrompeer to recover from connected mainnet peers
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
		t.Logf("PASS: magi-%d recovered pruned blocks from mainnet peers", recoveredOn)
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
		t.Errorf("saw %d 'giving up on pruned block recovery' log lines — some fetch attempts failed", gaveUp)
	}
	if noPeers > 0 {
		t.Errorf("saw %d 'no archive peers' log lines — peer filtering may be too aggressive", noPeers)
	}
}
