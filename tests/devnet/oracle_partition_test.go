package devnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// TestOracleChainRelayPartition reproduces the bug where down witness
// nodes prevent the BTC chain oracle from collecting enough BLS signatures
// to submit a relay transaction.
//
// Background
// ----------
// The producer broadcasts a chainRelayRequest to all elected witnesses
// and waits up to 90s for BLS signatures totaling more than 2/3 of the
// election weight (modules/oracle/chain/handle_block_tick.go,
// signatureCollectionTimeout = 90 * time.Second). It does NOT skip
// known-down nodes — every collection waits the full timeout if responding
// weight stays at-or-below the threshold.
//
// With 5 nodes of equal weight, totalWeight=5 and threshold=floor(5*2/3)=3.
// The loop condition is `for signedWeight <= threshold` (strict
// greater-than required), so the producer needs at least 4 distinct
// signatures (its own + 3 others) before it can submit. Disconnect 2 of
// 5 nodes and the producer can collect at most 3 signatures (itself plus
// the other 2 healthy nodes), which is exactly == threshold and the loop
// keeps waiting until the 90s timeout fires.
//
// What the test does
// ------------------
//  1. Starts a 5-node devnet plus a bitcoind regtest container.
//  2. Deploys the btc-stub mapping contract (modules/oracle treats it as
//     the BTC chain's mapping contract for the duration of the test).
//  3. Injects oracleConfig (RPC -> bitcoind:18443) into each node and
//     SysConfigOverrides.OracleParams.ChainContracts["BTC"] -> stub id.
//  4. Restarts magi nodes so they pick up the new oracle settings.
//  5. Mines BTC blocks via regtest so the oracle has something to relay.
//  6. Initializes the contract's "h" key (otherwise the producer skips
//     the chain — see fetchChainStatus in chain_relay.go).
//  7. Disconnects 2 of the 5 nodes — leaves 3 healthy = exactly threshold.
//  8. Waits up to 6 minutes (long enough for at least one full producer
//     rotation through a healthy node + the 90s collection timeout) for
//     the producer to log "not enough signatures for chain relay".
//  9. Asserts the log appeared AND no successful relay submission
//     happened during the partition window.
//
// Run with:
//
//	go test -v -run TestOracleChainRelayPartition -timeout 30m ./tests/devnet/
func TestOracleChainRelayPartition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet oracle partition test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	// ── Build the stub BTC mapping contract before starting the devnet ──

	stubWasm, err := BuildBtcStubContract(ctx)
	if err != nil {
		t.Fatalf("building btc-stub contract: %v", err)
	}

	// ── Devnet config ───────────────────────────────────────────────────

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.LogLevel = "info,oracle=debug,chain-relay=debug"
	cfg.EnableBitcoind = true

	// Tighten the election interval so we get a fresh election quickly.
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

	// ── Deploy the stub BTC mapping contract ────────────────────────────

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

	// ── Wire oracle config: bitcoind RPC + ChainContracts[BTC]=stub ─────

	if err := d.WriteOracleConfigs(ctx); err != nil {
		t.Fatalf("writing oracle configs: %v", err)
	}
	if err := d.SetOracleContractIDs(map[string]string{"BTC": contractId}); err != nil {
		t.Fatalf("setting ChainContracts: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("restarting magi nodes: %v", err)
	}

	// ── Mine some BTC blocks + initialize the contract's "h" key ────────
	//
	// The producer's fetchChainStatus() bails out if contract height is
	// 0 (chain_relay.go around line 361). We call our stub's `init`
	// action once to set h to (BTC tip - 5) so the producer's first
	// relay attempt will try to send blocks (tip-4 .. tip).

	if _, err := d.MineBlocks(ctx, 110); err != nil {
		t.Fatalf("mining initial BTC blocks: %v", err)
	}
	startHeight, err := d.BitcoinHeight(ctx)
	if err != nil {
		t.Fatalf("BitcoinHeight: %v", err)
	}
	t.Logf("bitcoind regtest height: %d", startHeight)

	// Set h = startHeight - 5 so the first relay attempt has 6 fresh blocks.
	initPayload := fmt.Sprintf(`{"start_height":%d}`, startHeight-5)
	if _, err := d.CallContract(ctx, 2, contractId, "init", initPayload); err != nil {
		t.Fatalf("init contract: %v", err)
	}

	// Wait for the init transaction to actually land — proven by any node
	// logging that it can read the contract state successfully. We accept
	// either:
	//   - "initiating chain relay consensus" — producer started a relay
	//   - "no new blocks to relay"           — producer read state, no work
	// Either confirms `getContractBlockHeight` is returning a valid number.
	t.Log("waiting for init transaction to land (contract state must become readable)...")
	initLanded := waitForLogAnyNode(t, d, ctx,
		"initiating chain relay consensus",
		3*time.Minute)
	if initLanded == 0 {
		// Fall back to "no new blocks" — also proves state is readable.
		initLanded = waitForLogAnyNode(t, d, ctx,
			"no new blocks to relay",
			30*time.Second)
	}
	if initLanded == 0 {
		dumpOracleDiagnostics(t, d, ctx)
		t.Fatalf("init transaction never landed — contract state still empty after 3.5 minutes (test setup is broken)")
	}
	t.Logf("init landed (magi-%d read contract state successfully)", initLanded)

	// ── Disconnect 2 nodes BEFORE any further successful relay ──────────
	//
	// 5 nodes => threshold = 5*2/3 = 3 (strict greater-than required).
	// Cut 2 out of 5 leaves 3 responders (producer + 2 witnesses) which
	// equals threshold but does NOT exceed it, so the producer's loop
	// waits the full 90s timeout per relay attempt.

	t.Log("partition: disconnecting magi-3 and magi-4...")
	if err := d.Disconnect(ctx, 3); err != nil {
		t.Fatalf("disconnect node 3: %v", err)
	}
	if err := d.Disconnect(ctx, 4); err != nil {
		t.Fatalf("disconnect node 4: %v", err)
	}

	// Capture baselines so we can measure changes during the partition.
	baseAttempts := countLogOccurrencesAllNodes(d, ctx, "initiating chain relay consensus")
	baseSuccess := countLogOccurrencesAllNodes(d, ctx, "chain relay transaction submitted")
	baseStalls := countLogOccurrencesAllNodes(d, ctx, "not enough signatures for chain relay")
	t.Logf("baseline counts — attempts:%d successes:%d stalls:%d",
		baseAttempts, baseSuccess, baseStalls)

	// Mine more BTC blocks so the producer always sees fresh work to do.
	if _, err := d.MineBlocks(ctx, 10); err != nil {
		t.Fatalf("mining BTC blocks during partition: %v", err)
	}

	// ── Wait for the relay-stall log line ───────────────────────────────
	//
	// Producer rotation needs to land on one of the 3 healthy nodes,
	// then that node needs to:
	//   1. Detect new BTC blocks (~3s tick)
	//   2. Broadcast signature request
	//   3. Collect 3 signatures (self + 2 witnesses)
	//   4. signedWeight (3) <= threshold (3) → keep waiting
	//   5. Hit the 90s timeout, log "not enough signatures for chain relay"
	//
	// Worst case wall-clock for one such cycle is ~2 minutes; we allow
	// 6 minutes total to be safe across multiple producer rotations.

	t.Log("waiting up to 6 minutes for the producer to log 'not enough signatures for chain relay'...")
	stalledOn := waitForLogAnyNode(t, d, ctx,
		"not enough signatures for chain relay",
		6*time.Minute)

	// Tally what actually happened during the partition.
	endAttempts := countLogOccurrencesAllNodes(d, ctx, "initiating chain relay consensus")
	endSuccess := countLogOccurrencesAllNodes(d, ctx, "chain relay transaction submitted")
	endStalls := countLogOccurrencesAllNodes(d, ctx, "not enough signatures for chain relay")

	deltaAttempts := endAttempts - baseAttempts
	deltaSuccess := endSuccess - baseSuccess
	deltaStalls := endStalls - baseStalls
	t.Logf("during partition — relay attempts:%d successful submits:%d stalls:%d",
		deltaAttempts, deltaSuccess, deltaStalls)

	dumpOracleDiagnostics(t, d, ctx)

	// Detection: it's only a meaningful test if at least one relay attempt
	// happened during the partition window. If the producer rotation never
	// landed on a healthy node we have no signal either way.
	if deltaAttempts == 0 {
		t.Fatalf("INCONCLUSIVE: no relay attempts during 6-minute partition window. The producer rotation may not have landed on a healthy node — try a longer wait or different node selection.")
	}

	if stalledOn == 0 {
		t.Errorf("BUG NOT REPRODUCED: %d relay attempts happened during the partition but none logged 'not enough signatures for chain relay'", deltaAttempts)
	} else {
		t.Logf("BUG REPRODUCED: magi-%d logged 'not enough signatures for chain relay' (saw %d such logs across all nodes)", stalledOn, deltaStalls)
	}

	if deltaSuccess > 0 {
		t.Errorf("UNEXPECTED RECOVERY: %d new relay submissions during partition. The bug appears to be partially mitigated.",
			deltaSuccess)
	}

	// ── Optional Phase C: reconnect and verify recovery ────────────────
	//
	// Skipped by default. Run with ORACLE_TEST_VERIFY_RECOVERY=1 to also
	// verify the relay resumes once nodes come back online.

	if os.Getenv("ORACLE_TEST_VERIFY_RECOVERY") != "" {
		t.Log("recovery phase: reconnecting nodes and verifying relay resumes...")
		d.Reconnect(ctx, 3)
		d.Reconnect(ctx, 4)

		if _, err := d.MineBlocks(ctx, 3); err != nil {
			t.Fatalf("mining BTC blocks: %v", err)
		}

		recoveredOn := waitForLogAnyNode(t, d, ctx,
			"chain relay transaction submitted",
			3*time.Minute)
		if recoveredOn == 0 {
			t.Errorf("relay never recovered after reconnecting nodes")
		} else {
			t.Logf("relay resumed (magi-%d) after reconnect", recoveredOn)
		}
	} else {
		// Reconnect for clean teardown.
		d.Reconnect(ctx, 3)
		d.Reconnect(ctx, 4)
	}
}

// waitForLogAnyNode polls every magi node's logs every 5 seconds for the
// given substring, returning the (1-indexed) node number on the first hit.
// Returns 0 if the timeout expires without a match.
func waitForLogAnyNode(t *testing.T, d *Devnet, ctx context.Context, substr string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for i := 1; i <= d.cfg.Nodes; i++ {
			if nodeLogsContain(d, ctx, i, substr) {
				return i
			}
		}
		if time.Now().After(deadline) {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(5 * time.Second):
		}
	}
}

// countLogOccurrencesAllNodes returns the total number of times substr
// appears across every magi node's logs.
func countLogOccurrencesAllNodes(d *Devnet, ctx context.Context, substr string) int {
	total := 0
	for i := 1; i <= d.cfg.Nodes; i++ {
		logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
		if err != nil {
			continue
		}
		total += strings.Count(logs, substr)
	}
	return total
}

// dumpOracleDiagnostics prints recent oracle-related log lines from every
// magi node so failing tests have something useful to grep.
func dumpOracleDiagnostics(t *testing.T, d *Devnet, ctx context.Context) {
	t.Helper()
	for i := 1; i <= d.cfg.Nodes; i++ {
		logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
		var oracleLines []string
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, "chain-relay") ||
				strings.Contains(line, "chain_relay") ||
				strings.Contains(line, "oracle") ||
				strings.Contains(line, "BTC") {
				oracleLines = append(oracleLines, line)
			}
		}
		if len(oracleLines) > 30 {
			oracleLines = oracleLines[len(oracleLines)-30:]
		}
		if len(oracleLines) > 0 {
			t.Logf("magi-%d oracle log tail:\n%s", i, strings.Join(oracleLines, "\n"))
		}
	}
}
