package devnet

// Multi-node devnet proof of the pendulum LP minimum-floor (consensus 0.2.0).
//
// The split that routes swap fees between node operators and liquidity
// providers is consensus-critical: every validator must compute the IDENTICAL
// floored split from the same geometry, or the chain forks. Unit + single-node
// ledger tests prove the math and the gating; only a real multi-node devnet
// proves all nodes agree on the floored split on-chain.
//
// Setup: an HBD-paired amm-pool publishes a huge HBD reserve (r0), which drives
// the geometry into the under-secured regime (V = 2*r0 >> c*E) where the raw
// pendulum routes ~100% of the pot to nodes. With the consensus floor pinned to
// 0.2.0 the LP minimum-floor must instead cap the node share at 75% (LPs keep
// >= 25%). The test fires a real swap through system.pendulum_apply_swap_fees
// and asserts, on a DIFFERENT node than the caller and across ALL nodes, that
// the split is ~75/25 and byte-identical everywhere.

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

func TestPendulumLPFloorDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pendulum LP-floor devnet test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	t.Cleanup(cancel)

	wasm, err := BuildAmmPoolContract(ctx)
	if err != nil {
		t.Fatalf("building amm-pool contract: %v", err)
	}

	cfg := DefaultConfig()
	// Pin the election floor to consensus 0.2.0 from epoch 1: every node runs
	// the 0.2.0 binary, so none are excluded and ActiveConsensusVersion >= 0.2.0
	// makes the LP minimum-floor active. Fast elections keep the run bounded.
	cfg.LogLevel = "error,pendulum=trace,oracle=trace"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorConsensus: 2,
			ElectionInterval:               20,
		},
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	t.Logf("starting %d-node devnet (~12 min)...", cfg.Nodes)
	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 1, 30, 8*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, c := context.WithTimeout(ctx, 10*time.Minute)
		if err := d.waitForElectionEpoch(nctx, n, 1, 10*time.Minute); err != nil {
			c()
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
		c()
	}

	// Deploy the pool, then whitelist it (its id isn't known until now) by
	// rewriting sysconfig and restarting all nodes.
	poolID, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "amm-pool", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy amm-pool: %v", err)
	}
	t.Logf("deployed pool=%s", poolID)

	if err := d.setPendulumWhitelistAndRestart(ctx, []string{poolID}); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("whitelisting pool + restart: %v", err)
	}

	// Chain must resume on every node after the restart, and the oracle feed
	// (in-memory) must re-warm before geometry is computable.
	head := nodeBlock(t, d, ctx, 1)
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.WaitForBlockProcessing(ctx, n, head+10, 8*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d did not resume after whitelist restart: %v", n, err)
		}
	}
	t.Log("nodes resumed post-whitelist-restart; warming oracle feed...")
	waitForFeedWarmup(t, d, ctx)

	// Fund the pool with real HBD (backs the node-share drain): deposit HBD to
	// witness-1, then have the pool draw 10 HBD via a transfer.allow intent.
	// Deposit 70 (witness-1 has ~90 TBD on L1 after the 10 TBD deploy fee) so it
	// covers the draw PLUS the RC/HBD reservation PullBalance applies
	// (rc_limit - free_rc, see below).
	if _, err := d.Deposit(ctx, 1, "70.000", "hbd"); err != nil {
		t.Fatalf("deposit hbd: %v", err)
	}
	if !waitForL2Balance(t, d, ctx, 1, witnessHive(d, 1), "hbd", 65_000, 4*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("witness-1 HBD deposit never credited on L2")
	}

	// Publish under-secured geometry: r0 (HBD reserve) huge so V = 2*r0 >> c*E.
	if _, err := d.CallContract(ctx, 1, poolID, "setup", "1000000000000,1000000000"); err != nil {
		t.Fatalf("setup call: %v", err)
	}
	if !waitStateKey(t, d, ctx, 2, poolID, "a0n", "hbd", 4*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("pool geometry (a0n=hbd) never published")
	}

	// Draw 15 HBD into the pool. CRITICAL: a low rc_limit (20000, ~2x the free
	// RC tier) — PullBalance reserves (rc_limit - free_rc) HBD of the caller's
	// balance against RC during an intent draw, so the default 500000 rc_limit
	// would reserve ~490 HBD and starve the 15 HBD draw.
	fundIntents := []map[string]interface{}{
		{"type": "transfer.allow", "args": map[string]string{"token": "hbd", "limit": "15.000"}},
	}
	if _, err := d.CallContractWithIntents(ctx, 1, poolID, "fund", "10000", fundIntents, 20000); err != nil {
		t.Fatalf("fund call: %v", err)
	}
	if !waitForStateKeyAtLeast(t, d, ctx, 2, poolID, "funded_hbd", 10_000, 4*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("pool never received drawn HBD (funded_hbd state key)")
	}

	// Fire the swap. A small swap keeps the node-share well under the funded
	// 10 HBD; the under-secured geometry + 0.2.0 floor cap it at 75%. Retry a
	// few times — right after the restart the in-memory oracle feed may not be
	// warm yet, so the first swap can revert with snapshot-unavailable.
	swapJSON := `{"asset_in":"hive","asset_out":"hbd","x":"50000","x_reserve":"1000000","y_reserve":"1000000"}`
	swapLanded := false
	for attempt := 0; attempt < 6 && !swapLanded; attempt++ {
		if _, err := d.CallContract(ctx, 1, poolID, "swap", swapJSON); err != nil {
			t.Fatalf("swap call: %v", err)
		}
		// Read the split on a DIFFERENT node (node 3): if nodes hadn't processed
		// it identically the keys would never appear.
		swapLanded = waitStateKeyPresent(t, d, ctx, 3, poolID, "node_share", 90*time.Second)
		if !swapLanded {
			t.Logf("swap attempt %d not recorded yet (feed warming?), retrying...", attempt+1)
		}
	}
	if !swapLanded {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("swap split never recorded — swap reverted (feed/geometry?) or nodes diverged")
	}

	// Read the split on every node and assert (a) it is byte-identical
	// everywhere (consensus determinism) and (b) it is the floored ~75/25.
	type split struct{ lp, node, sAfter, bucket int64 }
	splits := make([]split, cfg.Nodes)
	for n := 1; n <= cfg.Nodes; n++ {
		st, err := d.GetStateByKeys(ctx, n, poolID, []string{"lp_share", "node_share", "s_after", "node_bucket"})
		if err != nil {
			t.Fatalf("magi-%d read split: %v", n, err)
		}
		splits[n-1] = split{
			lp:     mustStateInt(t, n, st, "lp_share"),
			node:   mustStateInt(t, n, st, "node_share"),
			sAfter: mustStateInt(t, n, st, "s_after"),
			bucket: mustStateInt(t, n, st, "node_bucket"),
		}
		t.Logf("magi-%d: lp=%d node=%d s_after_bps=%d node_bucket=%d",
			n, splits[n-1].lp, splits[n-1].node, splits[n-1].sAfter, splits[n-1].bucket)
	}

	first := splits[0]
	for n := 2; n <= cfg.Nodes; n++ {
		if splits[n-1] != first {
			t.Fatalf("split diverged across nodes: magi-1=%+v magi-%d=%+v (consensus break)", first, n, splits[n-1])
		}
	}

	// Sanity: under-secured (s_after well past the cliff) and money actually moved.
	if first.sAfter <= 30_000 { // CliffSBps = 30000 (c = 3.0)
		t.Fatalf("expected under-secured geometry (s_after_bps > 30000), got %d", first.sAfter)
	}
	if first.node <= 0 || first.bucket <= 0 {
		t.Fatalf("expected a positive node share/accrual, got node=%d bucket=%d", first.node, first.bucket)
	}

	// The LP floor must bind: LPs keep ~25% of the pot, nodes capped at ~75%.
	pot := first.lp + first.node
	if pot <= 0 {
		t.Fatalf("empty pendulum pot")
	}
	quarter := pot / 4
	if first.lp < quarter-2 {
		t.Fatalf("LP floor not enforced: lp=%d is below ~25%% of pot=%d (under-secured swap)", first.lp, pot)
	}
	if first.node > pot-quarter+2 {
		t.Fatalf("node share=%d exceeds ~75%% cap of pot=%d", first.node, pot)
	}
	t.Logf("LP floor holds on-chain across %d nodes: pot=%d lp=%d (%.1f%%) node=%d (%.1f%%)",
		cfg.Nodes, pot, first.lp, 100*float64(first.lp)/float64(pot),
		first.node, 100*float64(first.node)/float64(pot))
}

// setPendulumWhitelistAndRestart rewrites the shared sysconfig.json with the
// given pool ids in PendulumPoolWhitelist (preserving the other overrides) and
// restarts every magi node so magid re-reads it. The pool id is only known
// after deploy, so this is the only way to whitelist it on devnet.
func (d *Devnet) setPendulumWhitelistAndRestart(ctx context.Context, pools []string) error {
	if d.cfg.SysConfigOverrides == nil {
		d.cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{}
	}
	list := append([]string(nil), pools...)
	d.cfg.SysConfigOverrides.PendulumPoolWhitelist = &list
	if err := writeSysConfigOverrides(d.cfg, d.devnetDir); err != nil {
		return fmt.Errorf("rewrite sysconfig: %w", err)
	}
	for n := 1; n <= d.cfg.Nodes; n++ {
		if err := d.StopNode(ctx, n); err != nil {
			return fmt.Errorf("stop magi-%d: %w", n, err)
		}
	}
	for n := 1; n <= d.cfg.Nodes; n++ {
		if err := d.StartNode(ctx, n); err != nil {
			return fmt.Errorf("start magi-%d: %w", n, err)
		}
	}
	return nil
}

func witnessHive(d *Devnet, node int) string {
	return "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, node)
}

// waitForFeedWarmup gives the in-memory oracle FeedTracker time to re-ingest
// feed_publish ops after the restart so geometry becomes computable. It waits
// for fresh feed_publish ops to appear in recent blocks on magi-1, then a grace
// period for the moving-average window to populate.
func waitForFeedWarmup(t *testing.T, d *Devnet, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		head := nodeBlock(t, d, ctx, 1)
		from := uint64(1)
		if head > 40 {
			from = head - 40
		}
		c, err := countHiveBlocksWithFeedPublishOps(ctx, d, 1, from, head)
		if err == nil && c > 0 {
			// Let a few more feeds land so the moving-average window is fresh.
			time.Sleep(45 * time.Second)
			t.Logf("oracle feed warm (%d recent feed_publish blocks)", c)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled during feed warmup: %v", ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}
	t.Log("warning: proceeding without confirmed feed warmup")
}

// waitForL2Balance polls until the account's L2 balance for the asset reaches
// minBaseUnits (base units, precision 3) on the given node.
func waitForL2Balance(t *testing.T, d *Devnet, ctx context.Context, node int, account, asset string, minBaseUnits int64, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		bal, err := d.GetAccountBalance(ctx, node, account)
		if err == nil && bal != nil {
			got := bal.Hive
			if asset == "hbd" {
				got = bal.Hbd
			}
			if got >= minBaseUnits {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
	return false
}

// waitForStateKeyAtLeast polls until a contract state key parses to an int >= min.
func waitForStateKeyAtLeast(t *testing.T, d *Devnet, ctx context.Context, node int, contractID, key string, min int64, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := d.GetStateByKeys(ctx, node, contractID, []string{key})
		if err == nil && st[key] != nil {
			if v, perr := strconv.ParseInt(fmt.Sprintf("%v", st[key]), 10, 64); perr == nil && v >= min {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
	return false
}

// waitStateKeyPresent polls until a contract state key has any non-nil value.
func waitStateKeyPresent(t *testing.T, d *Devnet, ctx context.Context, node int, contractID, key string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := d.GetStateByKeys(ctx, node, contractID, []string{key})
		if err == nil && st[key] != nil && fmt.Sprintf("%v", st[key]) != "" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
	return false
}

func mustStateInt(t *testing.T, node int, st map[string]any, key string) int64 {
	t.Helper()
	v, ok := st[key]
	if !ok || v == nil {
		t.Fatalf("magi-%d: state key %q absent", node, key)
	}
	n, err := strconv.ParseInt(fmt.Sprintf("%v", v), 10, 64)
	if err != nil {
		t.Fatalf("magi-%d: state key %q = %v not an int: %v", node, key, v, err)
	}
	return n
}
