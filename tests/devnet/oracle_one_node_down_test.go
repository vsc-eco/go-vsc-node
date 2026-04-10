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

// TestOracleChainRelayOneNodeDown is the sanity-check counterpart to
// TestOracleChainRelayPartition. It runs the exact same devnet setup
// (5 nodes, btc-stub, bitcoind regtest) but only disconnects ONE
// witness instead of two, then asserts that the oracle's chain relay
// keeps producing successful submissions.
//
// With 5 elected witnesses of equal weight:
//
//	totalWeight = 5, threshold = floor(5 * 2 / 3) = 3
//	loop condition: signedWeight <= 3 (strict greater-than required)
//
// Cutting one witness leaves 4 healthy nodes (producer + 3 witnesses),
// so signedWeight reaches 4 which is > 3 → the loop exits cleanly and
// the relay submits. No watchdog firing, no "not enough signatures"
// log.
//
// This test exists to confirm:
//  1. The fix's no-progress watchdog does NOT prematurely cancel a
//     healthy collection just because progress slowed momentarily.
//  2. The system tolerates the realistic 1-node-down scenario that
//     was actually observed on testnet (magi.test3 offline).
//
// Run with:
//
//	go test -v -run TestOracleChainRelayOneNodeDown -timeout 30m ./tests/devnet/
func TestOracleChainRelayOneNodeDown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet oracle one-node-down test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	// ── Build the stub BTC mapping contract ─────────────────────────────

	stubWasm, err := BuildBtcStubContract(ctx)
	if err != nil {
		t.Fatalf("building btc-stub contract: %v", err)
	}

	// ── Devnet config (identical to TestOracleChainRelayPartition) ──────

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.LogLevel = "info,oracle=debug,chain-relay=debug"
	cfg.EnableBitcoind = true

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

	// ── Deploy stub, wire oracle config, restart nodes ──────────────────

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

	if err := d.WriteOracleConfigs(ctx); err != nil {
		t.Fatalf("writing oracle configs: %v", err)
	}
	if err := d.SetOracleContractIDs(map[string]string{"BTC": contractId}); err != nil {
		t.Fatalf("setting ChainContracts: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("restarting magi nodes: %v", err)
	}

	// ── Mine BTC blocks + init the contract ─────────────────────────────

	if _, err := d.MineBlocks(ctx, 110); err != nil {
		t.Fatalf("mining initial BTC blocks: %v", err)
	}
	startHeight, err := d.BitcoinHeight(ctx)
	if err != nil {
		t.Fatalf("BitcoinHeight: %v", err)
	}
	t.Logf("bitcoind regtest height: %d", startHeight)

	initPayload := fmt.Sprintf(`{"start_height":%d}`, startHeight-5)
	if _, err := d.CallContract(ctx, 2, contractId, "init", initPayload); err != nil {
		t.Fatalf("init contract: %v", err)
	}

	// Wait for init to land — proven by any node logging "initiating chain
	// relay consensus" (producer started a relay) or "no new blocks to
	// relay" (producer read state cleanly with nothing to do).
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

	// ── Disconnect ONE node only ────────────────────────────────────────
	//
	// 5 nodes => threshold = floor(5*2/3) = 3 (strict greater-than).
	// Cutting just 1 node leaves 4 healthy nodes (producer + 3 witnesses),
	// so signedWeight = 4 > 3 → relay should succeed every cycle.

	t.Log("partition: disconnecting magi-3 (single node)...")
	if err := d.Disconnect(ctx, 3); err != nil {
		t.Fatalf("disconnect node 3: %v", err)
	}

	// Capture baselines so we can measure changes during the partition.
	baseAttempts := countLogOccurrencesAllNodes(d, ctx, "initiating chain relay consensus")
	baseSuccess := countLogOccurrencesAllNodes(d, ctx, "chain relay transaction submitted")
	baseStalls := countLogOccurrencesAllNodes(d, ctx, "not enough signatures for chain relay")
	t.Logf("baseline counts — attempts:%d successes:%d stalls:%d",
		baseAttempts, baseSuccess, baseStalls)

	// Mine more BTC blocks so the producer always has fresh work to do.
	if _, err := d.MineBlocks(ctx, 10); err != nil {
		t.Fatalf("mining BTC blocks during partition: %v", err)
	}

	// ── Wait for at least one successful relay submission ───────────────
	//
	// With 4/5 nodes healthy, the next producer rotation that lands on a
	// healthy node should submit successfully within ~30 seconds. Allow
	// up to 4 minutes to be safe across rotations.

	t.Log("waiting up to 4 minutes for the producer to submit a successful relay...")
	successOn := waitForLogAnyNode(t, d, ctx,
		"chain relay transaction submitted",
		4*time.Minute)

	endAttempts := countLogOccurrencesAllNodes(d, ctx, "initiating chain relay consensus")
	endSuccess := countLogOccurrencesAllNodes(d, ctx, "chain relay transaction submitted")
	endStalls := countLogOccurrencesAllNodes(d, ctx, "not enough signatures for chain relay")

	deltaAttempts := endAttempts - baseAttempts
	deltaSuccess := endSuccess - baseSuccess
	deltaStalls := endStalls - baseStalls
	t.Logf("during 1-node-down — relay attempts:%d successful submits:%d stalls:%d",
		deltaAttempts, deltaSuccess, deltaStalls)

	dumpOracleDiagnostics(t, d, ctx)

	if successOn == 0 {
		t.Errorf("UNEXPECTED STALL: no successful relay submitted within 4 minutes despite 4 of 5 nodes being healthy")
	} else {
		t.Logf("PASS: relay submitted by magi-%d with 1 node down (4 healthy)", successOn)
	}

	if deltaStalls > 0 {
		t.Errorf("UNEXPECTED STALL: saw %d 'not enough signatures' log lines during the 1-node-down period — the threshold should have been met", deltaStalls)
	} else {
		t.Logf("confirmed no stall logs during 1-node-down period")
	}

	// Reconnect for clean teardown.
	d.Reconnect(ctx, 3)
}
