package devnet

// Multi-node devnet proof of the THORChain swap-fee rule
// (params.ConsensusParams.PendulumSlipParityHeight): from the pinned height
// every node charges max(full slip fee x²·Y/(x+X)², protocol tier), with no
// 1% clamp, and all nodes agree on the result byte for byte.
//
// The swap: 50,000 into a 1,000,000 reserve (4.76% of the pool). Under the
// rule before it (1/16 slip leg + protocol tier, clamped at 1%) the fee is
// ~0.46% of the gross output; under THORChain's rule it is the full slip fee,
// ~4.76%.

import (
	"context"
	"encoding/json"
	"math/big"
	"strconv"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

func TestPendulumSlipParityDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pendulum slip-parity devnet test in short mode")
	}
	requireDocker(t)

	ctx, cancel := devnetWithTimeout(context.Background(), vfTestBudget(40*time.Minute))
	t.Cleanup(cancel)

	wasm, err := BuildAmmPoolContract(ctx)
	if err != nil {
		t.Fatalf("building amm-pool contract: %v", err)
	}

	cfg := DefaultConfig()
	cfg.LogLevel = "error,pendulum=trace,oracle=trace"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorConsensus: 2,
			ElectionInterval:               20,
			// THORChain's rule from the first block.
			PendulumSlipParityHeight: 1,
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
	if err := d.setPendulumWhitelistAndRestart(ctx, []string{poolID}); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("whitelisting pool + restart: %v", err)
	}
	head := nodeBlock(t, d, ctx, 1)
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.WaitForBlockProcessing(ctx, n, head+10, 8*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d did not resume after whitelist restart: %v", n, err)
		}
	}
	waitForFeedWarmup(t, d, ctx)

	// The node share leaves the pool as HBD, so the pool holds some (as in the
	// LP-floor test).
	if _, err := d.Deposit(ctx, 1, "70.000", "hbd"); err != nil {
		t.Fatalf("deposit hbd: %v", err)
	}
	if !waitForL2Balance(t, d, ctx, 1, witnessHive(d, 1), "hbd", 65_000, 4*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("witness-1 HBD deposit never credited on L2")
	}
	if _, err := d.CallContract(ctx, 1, poolID, "setup", "1000000,1000000"); err != nil {
		t.Fatalf("setup call: %v", err)
	}
	if !waitStateKey(t, d, ctx, 2, poolID, "a0n", "hbd", 4*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("pool geometry never published")
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

	const x, X, Y = 50_000, 1_000_000, 1_000_000
	swapJSON := `{"asset_in":"hive","asset_out":"hbd","x":"50000","x_reserve":"1000000","y_reserve":"1000000"}`
	landed := false
	for attempt := 0; attempt < 6 && !landed; attempt++ {
		if _, err := d.CallContract(ctx, 1, poolID, "swap", swapJSON); err != nil {
			t.Fatalf("swap call: %v", err)
		}
		landed = waitStateKeyPresent(t, d, ctx, 3, poolID, "user_output", 90*time.Second)
	}
	if !landed {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("swap never recorded (feed/geometry?) or nodes diverged")
	}

	// The same result on every node, once each node has processed the swap
	// (a node a block or two behind reads no result yet; one that never
	// shows it, or shows another, is a consensus break).
	var first string
	for n := 1; n <= cfg.Nodes; n++ {
		if !waitStateKeyPresent(t, d, ctx, n, poolID, "result", 3*time.Minute) {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d never recorded the swap result (consensus break or stalled node)", n)
		}
		st, err := d.GetStateByKeys(ctx, n, poolID, []string{"result"})
		if err != nil {
			t.Fatalf("magi-%d read result: %v", n, err)
		}
		got, _ := st["result"].(string)
		if n == 1 {
			first = got
		} else if got != first {
			t.Fatalf("result diverged: magi-1=%s magi-%d=%s (consensus break)", first, n, got)
		}
	}
	var res struct {
		UserOutput    string `json:"user_output"`
		MultiplierBps string `json:"multiplier_bps"`
		LpShare       string `json:"lp_share_output"`
		NodeShare     string `json:"node_share_output"`
		Network       string `json:"network_credit_output"`
	}
	if err := json.Unmarshal([]byte(first), &res); err != nil {
		t.Fatalf("decode result %q: %v", first, err)
	}
	num := func(n string) int64 { v, _ := strconv.ParseInt(n, 10, 64); return v }

	// The THORChain rule, with the applier's integer ops.
	bx, bX, bY := big.NewInt(x), big.NewInt(X), big.NewInt(Y)
	xPlusX := new(big.Int).Add(bx, bX)
	gross := new(big.Int).Div(new(big.Int).Mul(bx, bY), xPlusX)
	baseCLP := new(big.Int).Div(new(big.Int).Mul(new(big.Int).Mul(bx, bx), bY), new(big.Int).Mul(xPlusX, xPlusX))
	baseProtocol := new(big.Int).Div(new(big.Int).Mul(gross, big.NewInt(8)), big.NewInt(10_000))
	protocol := new(big.Int).Div(new(big.Int).Mul(baseProtocol, big.NewInt(num(res.MultiplierBps))), big.NewInt(10_000))
	if protocol.Cmp(baseProtocol) < 0 {
		protocol = baseProtocol
	}
	wantFee := baseCLP
	if protocol.Cmp(baseCLP) > 0 {
		wantFee = protocol
	}
	gotFee := gross.Int64() - num(res.UserOutput)
	t.Logf("gross %s, user output %d, fee %d (%.2f%%), THORChain rule %s, m=%s, lp=%d node=%d network=%d",
		gross, num(res.UserOutput), gotFee, 100*float64(gotFee)/float64(gross.Int64()), wantFee,
		res.MultiplierBps, num(res.LpShare), num(res.NodeShare), num(res.Network))
	if gotFee != wantFee.Int64() {
		t.Fatalf("fee %d, want max(slip fee %s, protocol tier %s) = %s", gotFee, baseCLP, protocol, wantFee)
	}
	if gotFee*100 <= gross.Int64() {
		t.Fatalf("fee %d is still within the old 1%% clamp of gross %s", gotFee, gross)
	}
	if split := num(res.LpShare) + num(res.NodeShare) + num(res.Network); split != gotFee {
		t.Fatalf("lp+node+network %d does not conserve to the fee %d", split, gotFee)
	}
	t.Logf("CASE SLIP-PARITY PASS: THORChain fee rule charged identically on all %d nodes", cfg.Nodes)
}
