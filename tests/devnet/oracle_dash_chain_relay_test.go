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

// TestOracleDashChainRelay verifies the oracle can relay Dash regtest block
// headers into the REAL dash-mapping-contract (the one in the sibling
// utxo-mapping repo) end-to-end.
//
// Differences from TestOraclePrunedBlockFetch:
//   - Uses the real dash-mapping-contract WASM, not a stub. The full
//     contract surface (seedBlocks, addBlocks, replaceBlocks, map, unmap,
//     transfer, …) is deployed and gated by checkAdmin.
//   - Uses dashd in regtest mode instead of bitcoind.
//   - No pruning — the goal is to verify the plain chain-relay path works
//     against the production contract.
//
// Setup:
//   - Start a 5-node magi devnet with dashd (regtest) as the oracle's
//     upstream RPC.
//   - Mine 5 blocks on dashd so the contract has something to chain onto.
//   - Deploy dash-mapping-contract. The deployer identity becomes
//     contract.owner, which satisfies checkAdmin on testnet mode (see
//     dash-mapping-contract/contract/main.go:50 checkAdmin).
//   - Call seedBlocks with block 1's header as the initial chain tip.
//     This seeds the contract's block store so addBlocks can chain from
//     there.
//   - Wire DASH into oracleConfig.json + SysConfigOverrides and restart
//     magi nodes so the oracle producer starts.
//   - Mine more dashd blocks. The oracle should detect them, collect
//     signatures from the 5 magi nodes, and submit addBlocks to the
//     contract.
//
// Assertions:
//   - A Dash chain relay transaction is successfully submitted (log
//     "chain relay transaction submitted" with symbol=DASH).
//   - At least one node logs "initiating chain relay consensus" for DASH.
//
// Prereqs:
//   - Built WASM at <utxo-mapping>/dash-mapping-contract/bin/testnet.wasm.
//     If you don't have it, build it with:
//
//	cd <utxo-mapping>/dash-mapping-contract && USE_DOCKER=1 make testnet
//
//     Or point DASH_MAPPING_WASM_PATH at an absolute path.
//
// Run with:
//
//	go test -v -run TestOracleDashChainRelay -timeout 25m ./tests/devnet/
func TestOracleDashChainRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet oracle Dash chain-relay test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// ── Locate the prebuilt dash-mapping-contract WASM ──────────────────
	wasmPath, err := DashMappingContractPath()
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Logf("using dash-mapping-contract WASM: %s", wasmPath)

	// ── Devnet config ───────────────────────────────────────────────────
	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.LogLevel = "info,oracle=debug,chain-relay=debug,oracle-dash=debug"
	cfg.EnableDashd = true

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

	// ── Mine a seed chain on dashd ──────────────────────────────────────
	//
	// Dash's Init() sets validityThreshold=1, so the oracle will relay
	// block N once dashd is at height >= N+1. Mine 5 blocks so there's
	// headroom for the oracle to pick up blocks 2, 3, 4, … after seeding.
	const seedHeight = 1
	const minedBlocks = 5
	if _, err := d.MineDashBlocks(ctx, minedBlocks); err != nil {
		t.Fatalf("mining initial dash blocks: %v", err)
	}
	tip, err := d.DashHeight(ctx)
	if err != nil {
		t.Fatalf("DashHeight: %v", err)
	}
	t.Logf("dashd tip after initial mine: %d", tip)

	seedHeaderHex, err := d.GetDashBlockHeaderHex(ctx, seedHeight)
	if err != nil {
		t.Fatalf("reading seed header at height %d: %v", seedHeight, err)
	}
	t.Logf("seed header (height=%d, hex=%s…)", seedHeight, seedHeaderHex[:32])

	// ── Deploy dash-mapping-contract ────────────────────────────────────
	t.Log("deploying dash-mapping-contract...")
	contractId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     wasmPath,
		Name:         "dash-mapping-contract",
		Description:  "Dash UTXO mapping contract (devnet)",
		DeployerNode: 1,
		GQLNode:      2,
	})
	if err != nil {
		t.Fatalf("deploying dash-mapping-contract: %v", err)
	}
	t.Logf("dash-mapping-contract deployed: %s", contractId)

	// ── Seed the contract with the initial block ────────────────────────
	//
	// On testnet builds, checkAdmin accepts contract.owner (the deployer).
	// The deployer identity belongs to magi-1, so we call from node 1.
	// The contract stores this as the last-known block; addBlocks expects
	// subsequent headers to chain onto this one via PrevBlock hash.
	seedPayload := fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, seedHeaderHex, seedHeight)
	t.Logf("calling seedBlocks(height=%d) from magi-1 (contract owner)...", seedHeight)
	if _, err := d.CallContract(ctx, 1, contractId, "seedBlocks", seedPayload); err != nil {
		t.Fatalf("seedBlocks: %v", err)
	}

	// ── Wire oracle configs and restart magi nodes ──────────────────────
	//
	// WriteOracleConfigs picks up EnableDashd=true and writes a DASH entry
	// into each node's oracleConfig.json. SetOracleContractIDs writes the
	// contract ID into SysConfigOverrides so the oracle knows where to
	// submit addBlocks.
	if err := d.WriteOracleConfigs(ctx); err != nil {
		t.Fatalf("writing oracle configs: %v", err)
	}
	if err := d.SetOracleContractIDs(map[string]string{"DASH": contractId}); err != nil {
		t.Fatalf("setting ChainContracts[DASH]: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("restarting magi nodes: %v", err)
	}

	// ── Wait for the oracle to notice + propose relay ───────────────────
	t.Log("waiting for Dash chain-relay consensus...")
	initiatedOn := waitForLogAnyNode(t, d, ctx,
		"initiating chain relay consensus",
		3*time.Minute)
	if initiatedOn == 0 {
		dumpOracleDiagnostics(t, d, ctx)
		t.Fatalf("no node initiated chain relay consensus — oracle may not be picking up DASH config")
	}
	t.Logf("magi-%d initiated chain relay consensus", initiatedOn)

	// Keep mining so there's ongoing work for the oracle to relay while
	// the initial signal lands on Hive and gets processed.
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = d.MineDashBlocks(ctx, 2)
			}
		}
	}()

	// ── Wait for a successful relay submission ──────────────────────────
	submittedOn := waitForLogAnyNode(t, d, ctx,
		"chain relay transaction submitted",
		4*time.Minute)

	dumpOracleDiagnostics(t, d, ctx)

	if submittedOn == 0 {
		t.Fatalf("FAIL: no successful Dash chain relay submission — oracle → real dash-mapping-contract pipeline broken")
	}
	t.Logf("PASS: magi-%d submitted Dash chain relay transaction", submittedOn)

	// Sanity: if we see repeated "contract is paused" / "bad_input" errors
	// then the contract likely rejected addBlocks for reasons other than
	// chain progress — surface that to the reader.
	paused := countLogOccurrencesAllNodes(d, ctx, "contract is paused")
	stalled := countLogOccurrencesAllNodes(d, ctx, "not enough signatures for chain relay")
	if paused > 0 {
		t.Errorf("WARN: %d 'contract is paused' log lines — paused flag set on contract?", paused)
	}
	if stalled > 0 {
		t.Logf("INFO: %d 'not enough signatures' log lines — some rounds stalled but eventually advanced", stalled)
	}
}
