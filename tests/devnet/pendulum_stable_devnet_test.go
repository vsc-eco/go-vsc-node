package devnet

// Multi-node devnet proof of the StableSwap pool (consensus 0.13.0).
//
// A pool listed in StablePools prices its swaps on the StableSwap curve. That
// output is consensus: every validator must compute the identical big.Int
// Newton result, or the chain forks. Unit and single-node contract tests
// prove the math, the gate and the contract floor; only a multi-node devnet
// proves every node agrees on-chain. Here the chain is pinned to 0.13.0, the
// devnet sysconfig lists the amm-pool as stable (HBD/HIVE, 3/3 decimals,
// A=100), and a stable swap must land with a byte-identical result on every
// node, priced on the curve (near par, far above the constant product).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

func TestPendulumStablePoolDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stable pool devnet test in short mode")
	}
	requireDocker(t)

	ctx, cancel := devnetWithTimeout(context.Background(), vfTestBudget(50*time.Minute))
	t.Cleanup(cancel)

	wasm, err := BuildAmmPoolContract(ctx)
	if err != nil {
		t.Fatalf("building amm-pool contract: %v", err)
	}

	cfg := DefaultConfig()
	cfg.LogLevel = "error,pendulum=trace,oracle=trace"
	// Pin the election floor to 0.13.0 from epoch 1 (every node runs this
	// binary, which announces 0.13.0): StableSwapActive from the start.
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorConsensus: 13,
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
		nctx, c := devnetWithTimeout(ctx, 10*time.Minute)
		if err := d.waitForElectionEpoch(nctx, n, 1, 10*time.Minute); err != nil {
			c()
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
		c()
	}

	poolID, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "amm-pool", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy amm-pool: %v", err)
	}
	t.Logf("deployed pool=%s", poolID)

	// List the pool: swap whitelist + StablePools (devnet takes both from
	// sysconfig; mainnet/testnet bake StablePools into the build).
	stable := []systemconfig.StablePool{{
		ID: poolID, FromHeight: 0, Amp: 100,
		Asset0: "hbd", Decimals0: 3, Asset1: "hive", Decimals1: 3,
	}}
	d.cfg.SysConfigOverrides.StablePools = &stable
	if err := d.setPendulumWhitelistAndRestart(ctx, []string{poolID}); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("listing pool + restart: %v", err)
	}
	head := nodeBlock(t, d, ctx, 1)
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.WaitForBlockProcessing(ctx, n, head+10, 8*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d did not resume after the restart: %v", n, err)
		}
	}
	waitForFeedWarmup(t, d, ctx)

	if _, err := d.Deposit(ctx, 1, "70.000", "hbd"); err != nil {
		t.Fatalf("deposit hbd: %v", err)
	}
	if !waitForL2Balance(t, d, ctx, 1, witnessHive(d, 1), "hbd", 65_000, 4*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("witness-1 HBD deposit never credited on L2")
	}
	// Balanced geometry (not the cliff): r0 = r1 = 1,000,000 HBD/HIVE.
	if _, err := d.CallContract(ctx, 1, poolID, "setup", "1000000000,1000000000"); err != nil {
		t.Fatalf("setup call: %v", err)
	}
	if !waitStateKey(t, d, ctx, 2, poolID, "a0n", "hbd", 4*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("pool state never published")
	}
	fundIntents := []map[string]interface{}{
		{"type": "transfer.allow", "args": map[string]string{"token": "hbd", "limit": "15.000"}},
	}
	if _, err := d.CallContractWithIntents(ctx, 1, poolID, "fund", "10000", fundIntents, 20000); err != nil {
		t.Fatalf("fund call: %v", err)
	}
	if !waitForStateKeyAtLeast(t, d, ctx, 2, poolID, "funded_hbd", 10_000, 4*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("pool never received drawn HBD")
	}

	// A 10% HIVE -> HBD swap on the stable curve: 100,000 in against 1,000,000
	// reserves. The constant product would pay ~90,909; the curve ~99,9xx.
	swapJSON := `{"asset_in":"hive","asset_out":"hbd","x":"100000","x_reserve":"1000000","y_reserve":"1000000",` +
		`"curve":"stable","amp":"100","dec_in":"3","dec_out":"3"}`
	landed := false
	for attempt := 0; attempt < 6 && !landed; attempt++ {
		if _, err := d.CallContract(ctx, 1, poolID, "swap", swapJSON); err != nil {
			t.Fatalf("swap call: %v", err)
		}
		landed = waitStateKey(t, d, ctx, 3, poolID, "curve", "stable", 90*time.Second)
		if !landed {
			t.Logf("stable swap attempt %d not recorded yet (feed warming?), retrying...", attempt+1)
		}
	}
	if !landed {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("stable swap never recorded: refused (gate/listing/feed?) or nodes diverged")
	}

	// Every node must record the swap itself; each may be a block or two behind
	// magi-3 (11-03 first run: magi-1 read the same second as magi-3 confirmed and
	// had not applied the block yet). A node that never records it in 3 min is a
	// real failure (refused it, or stalled).
	for n := 1; n <= cfg.Nodes; n++ {
		if !waitStateKey(t, d, ctx, n, poolID, "curve", "stable", 3*time.Minute) {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d never recorded the stable swap within 3 min of magi-3: refused it (listing/gate) or stalled", n)
		}
	}
	results := make([]string, cfg.Nodes)
	for n := 1; n <= cfg.Nodes; n++ {
		st, err := d.GetStateByKeys(ctx, n, poolID, []string{"result", "user_output", "curve"})
		if err != nil {
			t.Fatalf("magi-%d read result: %v", n, err)
		}
		results[n-1] = fmt.Sprintf("%v", st["result"])
		t.Logf("magi-%d: %s", n, results[n-1])
		if fmt.Sprintf("%v", st["curve"]) != "stable" {
			t.Fatalf("magi-%d: curve %v, want stable", n, st["curve"])
		}
		out := mustStateInt(t, n, st, "user_output")
		if out < 99_000 || out >= 100_000 {
			t.Fatalf("magi-%d: user_output %d for 100,000 in: not priced on the stable curve (constant product ~90,909)", n, out)
		}
	}
	for n := 2; n <= cfg.Nodes; n++ {
		if results[n-1] != results[0] {
			t.Fatalf("stable swap result diverged: magi-1=%s magi-%d=%s (consensus break)", results[0], n, results[n-1])
		}
	}
	t.Logf("stable swap priced identically on all %d nodes: %s", cfg.Nodes, results[0])

	// SP-0d: reserves past int64 (1e21 base units each, an 18-decimal pool's
	// scale: the bug refused every swap once a reserve passed ~9.22 ETH),
	// legal from 0.13.0. Same check: identical on every node, priced. The
	// swap itself stays small: the host pays the node share in real HBD out
	// of the pool's ledger balance (10 HBD funded above), and a 1e20 swap's
	// share is ~3e16 HBD units, so that payment fails and the swap aborts
	// (11-03 run 2: "never landed"). 1e6 in on 1e21 reserves needs a few
	// hundred units. Landed = the result carries the exact new reserve.
	bigJSON := `{"asset_in":"hive","asset_out":"hbd","x":"1000000","x_reserve":"1000000000000000000000",` +
		`"y_reserve":"1000000000000000000000","curve":"stable","amp":"100","dec_in":"3","dec_out":"3"}`
	const bigNewX = `"new_x_reserve":"1000000000000001000000"`
	bigLanded := func(n int) bool {
		st, err := d.GetStateByKeys(ctx, n, poolID, []string{"result"})
		return err == nil && strings.Contains(fmt.Sprintf("%v", st["result"]), bigNewX)
	}
	landed = false
	for attempt := 0; attempt < 6 && !landed; attempt++ {
		if _, err := d.CallContract(ctx, 1, poolID, "swap", bigJSON); err != nil {
			t.Fatalf("big swap call: %v", err)
		}
		for deadline := devnetNow().Add(90 * time.Second); devnetNow().Before(deadline) && !landed; devnetSleep(5 * time.Second) {
			landed = bigLanded(3)
		}
	}
	if !landed {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("the swap with reserves past int64 never landed (SP-0d)")
	}
	for n := 1; n <= cfg.Nodes; n++ {
		ok := false
		for deadline := devnetNow().Add(3 * time.Minute); devnetNow().Before(deadline) && !ok; devnetSleep(5 * time.Second) {
			ok = bigLanded(n)
		}
		if !ok {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d never recorded the swap past int64 within 3 min of magi-3 (SP-0d)", n)
		}
		st, err := d.GetStateByKeys(ctx, n, poolID, []string{"result"})
		if err != nil {
			t.Fatalf("magi-%d read big result: %v", n, err)
		}
		results[n-1] = fmt.Sprintf("%v", st["result"])
	}
	for n := 2; n <= cfg.Nodes; n++ {
		if results[n-1] != results[0] {
			t.Fatalf("big-amount swap diverged: magi-1=%s magi-%d=%s (consensus break)", results[0], n, results[n-1])
		}
	}
	t.Logf("swap past int64 priced identically on all %d nodes: %s", cfg.Nodes, results[0])
}
