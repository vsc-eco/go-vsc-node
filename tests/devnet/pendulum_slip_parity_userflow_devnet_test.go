//go:build evm_devnet

// USER FLOW under THORChain's swap-fee rule (PendulumSlipParityHeight), on the
// DEX3 devnet (real dex + dex-router-v2 wasm, BTC and EVM bridges).
//
// Every swap goes the way Altera sends it: Altera's own quote code
// (src/lib/pools/swapCalc.ts, run from ALTERA_DIR) prices it from the pool's
// live reserves and sets min_amount_out at 1% slippage; the router executes it.
// Each swap is then checked against the pool's own fee log (lp, ns, nc, nb, m):
//   - fee = max(slip fee x²·Y/(x+X)², protocol tier 8 bps × m), no clamp
//     (x/thorchain/swap_current.go GetSwapCalc, with our protocol tier as floor)
//   - network = 25% of each leg, lp + nodes + network = fee
//   - the user receives gross - fee, at least Altera's quote and its minimum
//   - the stored reserves move exactly as the contract and applier say
// UF3 is the deploy-order proof: a quote made with TODAY's rule ('off', what
// alterawallet.com runs now) on a large swap is refused by the new node rule
// ("slippage tolerance exceeded") and the trader keeps the HBD; the same swap
// quoted with 'both' (the transition release) goes through.
//
// Each case logs one "USERFLOW {json}" line (X, x, Y, fee legs) that
// /mnt/o/SWAP-SAFETY-2026-11-14/tools runs through THORChain's own code.
//
// Run: ALTERA_DIR=<altera worktree with the fee-rule mirror> plus the DEX3 env
// (BTC_MAPPING_WASM_PATH, DEX_WASM, DEX_ROUTER_WASM, EVM_*):
//   go test -tags evm_devnet -run TestPendulumSlipParityUserFlow ./tests/devnet/ -timeout 200m -v

package devnet

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

type ufQuote struct {
	ExpectedOutput string `json:"expectedOutput"`
	MinAmountOut   string `json:"minAmountOut"`
	TotalFee       string `json:"totalFee"`
	FeeBps         int64  `json:"feeBps"`
	GuardBlocks    bool   `json:"guardBlocks"`
}

// ufAltera runs Altera's quote code on req and returns its answer.
func ufAltera(t *testing.T, req map[string]any) ufQuote {
	t.Helper()
	dir := os.Getenv("ALTERA_DIR")
	b, _ := json.Marshal(req)
	res := filepath.Join(t.TempDir(), "quote.json")
	cmd := exec.Command("pnpm", "exec", "vitest", "run", "src/lib/pools/devnetQuote.test.ts")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "QUOTE_REQ="+string(b), "QUOTE_RES="+res, "DEV_BEHIND_PROXY=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("altera quote %s: %v\n%s", b, err, out)
	}
	raw, err := os.ReadFile(res)
	if err != nil {
		t.Fatalf("altera quote %s wrote nothing: %v", b, err)
	}
	var q ufQuote
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatal(err)
	}
	return q
}

func ufInt(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// ufReserves reads a pool's stored reserves r0/r1 as Altera does (hex).
func ufReserves(t *testing.T, x *dex3, pool string) (int64, int64) {
	t.Helper()
	st, err := getStateHex(x.b.d, x.b.ctx, x.b.e.gqlNode, pool, []string{"r0", "r1"})
	if err != nil {
		t.Fatal(err)
	}
	return new(big.Int).SetBytes(st["r0"]).Int64(), new(big.Int).SetBytes(st["r1"]).Int64()
}

type ufFeeLog struct{ lp, ns, nc, nb, m int64 }

// ufPoolFeeLog finds the pool's fee log for txID in node 1's contract_state.
func ufPoolFeeLog(t *testing.T, x *dex3, pool, txID string) ufFeeLog {
	t.Helper()
	d, ctx := x.b.d, x.b.ctx
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(ctx)
	coll := client.Database(d.nodeDbName(1)).Collection("contract_state")
	start := devnetNow()
	for devnetSince(start) < 3*time.Minute {
		var doc struct {
			Inputs  []string `bson:"inputs"`
			Results []struct {
				Logs []string `bson:"logs"`
			} `bson:"results"`
		}
		if err := coll.FindOne(ctx, bson.M{"contract_id": pool, "inputs": txID}).Decode(&doc); err == nil {
			for i, in := range doc.Inputs {
				if in != txID || i >= len(doc.Results) {
					continue
				}
				for _, l := range doc.Results[i].Logs {
					if !strings.HasPrefix(l, "fee|") {
						continue
					}
					kv := map[string]int64{}
					for _, part := range strings.Split(l, "|")[1:] {
						if k, v, ok := strings.Cut(part, "="); ok {
							kv[k], _ = strconv.ParseInt(v, 10, 64)
						}
					}
					return ufFeeLog{lp: kv["lp"], ns: kv["ns"], nc: kv["nc"], nb: kv["nb"], m: kv["m"]}
				}
			}
		}
		devnetSleep(5 * time.Second)
	}
	t.Fatalf("no fee log for %s on pool %s", txID, pool)
	return ufFeeLog{}
}

// ufCheckHop checks one pool hop against the THORChain rule and the
// contract's reserve update, and logs the USERFLOW line.
func ufCheckHop(t *testing.T, name, assetOut string, X, x, Y int64, f ufFeeLog, u int64, after [2]int64, fails *int) {
	t.Helper()
	bad := func(format string, a ...any) {
		*fails++
		t.Errorf("UF FAIL %s: "+format, append([]any{name}, a...)...)
	}
	bx, bX, bY := big.NewInt(x), big.NewInt(X), big.NewInt(Y)
	xX := new(big.Int).Add(bx, bX)
	gross := new(big.Int).Div(new(big.Int).Mul(bx, bY), xX).Int64()
	baseCLP := new(big.Int).Div(new(big.Int).Mul(new(big.Int).Mul(bx, bx), bY), new(big.Int).Mul(xX, xX)).Int64()
	baseP := gross * 8 / 10_000
	prot := baseP * f.m / 10_000
	if prot < baseP {
		prot = baseP
	}
	want := baseCLP
	if prot > want {
		want = prot
	}
	fee := f.lp + f.ns + f.nc
	dust := want == 0 && fee == 1 && f.nc == 1
	if fee != want && !dust {
		bad("fee %d, THORChain rule max(slip %d, protocol tier %d) = %d", fee, baseCLP, prot, want)
	}
	clp := want - prot
	if clp < 0 {
		clp = 0
	}
	if !dust && f.nc != clp/4+prot/4 {
		bad("network %d, want 25%% of each leg = %d", f.nc, clp/4+prot/4)
	}
	if u != gross-fee {
		bad("user output %d, want gross %d - fee %d = %d", u, gross, fee, gross-fee)
	}
	// Reserves as the pool stores them (after = [input side, output side]).
	if assetOut == "hbd" {
		if after[0] != X+x || after[1] != Y-u-f.ns-f.nc {
			bad("reserves %v, want [%d %d]", after, X+x, Y-u-f.ns-f.nc)
		}
	} else if after[0] != X+x-f.nb || after[1] != Y-u-f.nc {
		bad("reserves %v, want [%d %d]", after, X+x-f.nb, Y-u-f.nc)
	}
	line, _ := json.Marshal(map[string]any{"case": name, "asset_out": assetOut, "X": X, "x": x, "Y": Y, "gross": gross,
		"base_clp": baseCLP, "protocol": prot, "m": f.m, "fee": fee, "lp": f.lp, "nodes": f.ns, "network": f.nc, "node_bucket_hbd": f.nb, "user": u})
	t.Logf("USERFLOW %s", line)
	t.Logf("UF %s: gross %d, fee %d (%.2f%% of gross; slip %d, protocol tier %d), lp %d, nodes %d, network %d", name, gross, fee,
		100*float64(fee)/float64(gross), baseCLP, prot, f.lp, f.ns, f.nc)
}

func TestPendulumSlipParityUserFlow(t *testing.T) {
	if os.Getenv("ALTERA_DIR") == "" {
		t.Skip("ALTERA_DIR (Altera worktree with the fee-rule mirror) required")
	}
	parity := func(c *Config) { c.SysConfigOverrides.ConsensusParams.PendulumSlipParityHeight = 1 }
	x := newBothEnvDEX3(t, 180*time.Minute, parity)
	x.setup()
	b := x.b
	d, ctx, e := b.d, b.ctx, b.e
	trader := dex3Trader
	fails := 0
	hbdRec := func() *BalanceRecord {
		bal, err := d.GetAccountBalance(ctx, e.gqlNode, trader)
		if err != nil {
			t.Fatal(err)
		}
		return bal
	}
	hbd := func() int64 { return hbdRec().Hbd }
	// getAccountBalance serves the snapshot the state engine writes once per
	// slot (10 blocks), and only for accounts that changed. hbdChanged waits for
	// the trader's HBD to move off `before`; hbdUnchanged waits two slots past
	// now, after which a change would have been written, and reads it.
	hbdChanged := func(before int64) int64 {
		start := devnetNow()
		for devnetSince(start) < 3*time.Minute {
			if v := hbd(); v != before {
				return v
			}
			devnetSleep(5 * time.Second)
		}
		return hbd()
	}
	hbdUnchanged := func() int64 {
		head := nodeBlock(t, d, ctx, e.gqlNode)
		start := devnetNow()
		for nodeBlock(t, d, ctx, e.gqlNode) < head+20 && devnetSince(start) < 5*time.Minute {
			devnetSleep(5 * time.Second)
		}
		return hbd()
	}
	sats := func() int64 { return balanceSats(t, d, ctx, b.v.cid, trader) }
	eth := func() *big.Int {
		v, _ := new(big.Int).SetString(strings.SplitN(e.stateStr("a-"+trader+"-eth"), "|", 2)[0], 10)
		if v == nil {
			return new(big.Int)
		}
		return v
	}
	// execute sends the instruction as Altera does and returns the tx id and
	// whether the router accepted it.
	execute := func(instr string, hbdIn int64) (string, bool, string) {
		var id string
		var err error
		if hbdIn > 0 {
			id, err = d.CallContractWithIntents(ctx, 1, x.router, "execute", instr, hbdAllow(hbdIn), 50_000)
		} else {
			id, err = d.CallContract(ctx, 1, x.router, "execute", instr)
		}
		if err != nil {
			t.Fatalf("execute %s: %v", instr, err)
		}
		ok, msg := e.outResult(ctx, id)
		return id, ok, msg
	}
	swapInstr := func(in, out string, amount, min int64) string {
		return fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"%s","asset_out":"%s","amount_in":"%d","min_amount_out":"%d","recipient":"%s"}`,
			in, out, amount, min, trader)
	}
	single := func(xv, X, Y int64, rule string) ufQuote {
		return ufAltera(t, map[string]any{"kind": "single", "x": strconv.FormatInt(xv, 10), "X": strconv.FormatInt(X, 10), "Y": strconv.FormatInt(Y, 10), "slippageBps": 100, "rule": rule})
	}

	usdc := func() *big.Int {
		v, _ := new(big.Int).SetString(strings.SplitN(e.stateStr("a-"+trader+"-usdc"), "|", 2)[0], 10)
		if v == nil {
			return new(big.Int)
		}
		return v
	}
	bal := func(asset string) int64 {
		switch asset {
		case "hbd":
			return hbd()
		case "btc":
			return sats()
		case "eth":
			return eth().Int64()
		}
		return usdc().Int64()
	}
	// singleHop runs one Altera-quoted router swap on pool (assets a0/a1 as
	// the pool orders them) and checks it.
	singleHop := func(name, pool, a0, a1, in, out string, amount int64, rule string, expectRefusal bool) {
		r0, r1 := ufReserves(t, x, pool)
		X, Y := r0, r1
		if in == a1 {
			X, Y = r1, r0
		}
		q := single(amount, X, Y, rule)
		t.Logf("UF %s: Altera '%s' quote %d %s -> expected %s %s, min %s, fee %d bps, guard blocks %v", name, rule, amount, in, q.ExpectedOutput, out, q.MinAmountOut, q.FeeBps, q.GuardBlocks)
		inBefore, outBefore := bal(in), bal(out)
		if in == "hbd" || out == "hbd" {
			// Let the previous swap's HBD snapshot land before reading "before".
			h := hbdUnchanged()
			if in == "hbd" {
				inBefore = h
			} else {
				outBefore = h
			}
		}
		hbdIn := int64(0)
		if in == "hbd" {
			hbdIn = amount
		}
		id, ok, msg := execute(swapInstr(in, out, amount, ufInt(q.MinAmountOut)), hbdIn)
		if expectRefusal {
			if ok {
				fails++
				t.Errorf("UF FAIL %s: a stale '%s' minimum was accepted", name, rule)
			}
			if !strings.Contains(msg, "slippage") {
				fails++
				t.Errorf("UF FAIL %s: refused for %q, want the slippage check", name, msg)
			}
			got := bal(in)
			if in == "hbd" {
				got = hbdUnchanged()
			}
			if got != inBefore {
				fails++
				t.Errorf("UF FAIL %s: refused swap moved the trader's %s %d -> %d", name, in, inBefore, got)
			}
			t.Logf("UF %s: refused as expected (%s); trader keeps %d %s", name, msg, inBefore, in)
			return
		}
		if !ok {
			fails++
			t.Errorf("UF FAIL %s: router refused: %s", name, msg)
			return
		}
		if q.GuardBlocks {
			fails++
			t.Errorf("UF FAIL %s: Altera's fee guard would block this swap in the UI (%d bps, rule %s)", name, q.FeeBps, rule)
		}
		readBal := func(asset string) int64 {
			if asset == "hbd" {
				prev := inBefore
				if asset == out {
					prev = outBefore
				}
				return hbdChanged(prev)
			}
			return bal(asset)
		}
		start := devnetNow()
		for readBal(out) == outBefore && devnetSince(start) < 3*time.Minute {
			devnetSleep(5 * time.Second)
		}
		u := readBal(out) - outBefore
		if paid := inBefore - readBal(in); paid != amount {
			fails++
			t.Errorf("UF FAIL %s: trader paid %d %s, want %d", name, paid, in, amount)
		}
		if u < ufInt(q.MinAmountOut) || u < ufInt(q.ExpectedOutput)-1 {
			fails++
			t.Errorf("UF FAIL %s: received %d %s, Altera quoted %s (min %s)", name, u, out, q.ExpectedOutput, q.MinAmountOut)
		}
		t.Logf("UF %s: received %d, Altera expected %s (gap %d), min %s", name, u, q.ExpectedOutput, ufInt(q.ExpectedOutput)-u, q.MinAmountOut)
		a0r, a1r := ufReserves(t, x, pool)
		after := [2]int64{a0r, a1r}
		if in == a1 {
			after = [2]int64{a1r, a0r}
		}
		ufCheckHop(t, name, out, X, amount, Y, ufPoolFeeLog(t, x, pool, id), u, after, &fails)
	}

	// UF1: small swap, 0.1% of the HBD side of the HBD/USDC pool (50 HBD :
	// 20 USDC): slip ~10 bps is below the protocol tier, so the tier is the fee.
	singleHop("UF1 small HBD->USDC", x.poolUSDC, "hbd", "usdc", "hbd", "usdc", 50, "on", false)
	// UF2: large, 10% of the BTC pool's HBD side: the full slip fee (~9%),
	// uncapped; Altera's 10% guard lets it through (today's 3% guard would not).
	singleHop("UF2 large HBD->BTC", x.poolBTC, "btc", "hbd", "hbd", "btc", 5_000, "on", false)
	// UF3: today's Altera quote ('off', alterawallet.com now) on a large swap
	// meets the new node rule: refused, nothing moves.
	singleHop("UF3 stale 'off' quote", x.poolBTC, "btc", "hbd", "hbd", "btc", 3_000, "off", true)
	// UF4: the transition release's quote ('both') on the same swap.
	singleHop("UF4 'both' quote", x.poolBTC, "btc", "hbd", "hbd", "btc", 3_000, "both", false)
	// UF5: BTC -> HBD, HBD output (the node share leaves the pool as HBD).
	singleHop("UF5 BTC->HBD", x.poolBTC, "btc", "hbd", "btc", "hbd", 60_000, "on", false)

	// UF6: two hops, BTC -> HBD -> ETH, quoted by calculateTwoHopSwap.
	{
		b0, b1 := ufReserves(t, x, x.poolBTC)
		e0, e1 := ufReserves(t, x, x.poolETH)
		amount := int64(20_000)
		q := ufAltera(t, map[string]any{"kind": "two", "x": strconv.FormatInt(amount, 10),
			"pool1":   map[string]string{"asset0": "btc", "asset1": "hbd", "reserve0": strconv.FormatInt(b0, 10), "reserve1": strconv.FormatInt(b1, 10)},
			"pool2":   map[string]string{"asset0": "eth", "asset1": "hbd", "reserve0": strconv.FormatInt(e0, 10), "reserve1": strconv.FormatInt(e1, 10)},
			"assetIn": "btc", "hop": "hbd", "assetOut": "eth", "slippageBps": 100, "rule": "on"})
		if q.GuardBlocks {
			fails++
			t.Errorf("UF FAIL UF6: Altera's fee guard would block this swap in the UI (%d bps)", q.FeeBps)
		}
		ethBefore, sBefore := eth(), sats()
		id, ok, msg := execute(swapInstr("btc", "eth", amount, ufInt(q.MinAmountOut)), 0)
		if !ok {
			fails++
			t.Errorf("UF FAIL UF6: router refused: %s", msg)
		} else {
			start := devnetNow()
			for eth().Cmp(ethBefore) == 0 && devnetSince(start) < 3*time.Minute {
				devnetSleep(5 * time.Second)
			}
			u := new(big.Int).Sub(eth(), ethBefore).Int64()
			if sBefore-sats() != amount {
				fails++
				t.Errorf("UF FAIL UF6: trader paid %d sats, want %d", sBefore-sats(), amount)
			}
			exp := ufInt(q.ExpectedOutput)
			if u < ufInt(q.MinAmountOut) || u < exp-exp/500 {
				fails++
				t.Errorf("UF FAIL UF6: received %d wei, Altera quoted %s (min %s)", u, q.ExpectedOutput, q.MinAmountOut)
			}
			t.Logf("UF UF6: received %d wei, Altera expected %d (gap %.3f%%), min %s", u, exp, 100*float64(exp-u)/float64(exp), q.MinAmountOut)
			f1 := ufPoolFeeLog(t, x, x.poolBTC, id)
			f2 := ufPoolFeeLog(t, x, x.poolETH, id)
			hop1Gross := new(big.Int).Div(new(big.Int).Mul(big.NewInt(amount), big.NewInt(b1)), big.NewInt(b0+amount)).Int64()
			hop1Out := hop1Gross - (f1.lp + f1.ns + f1.nc)
			a0, a1 := ufReserves(t, x, x.poolBTC)
			ufCheckHop(t, "UF6 hop1 BTC->HBD", "hbd", b0, amount, b1, f1, hop1Out, [2]int64{a0, a1}, &fails)
			c0, c1 := ufReserves(t, x, x.poolETH)
			ufCheckHop(t, "UF6 hop2 HBD->ETH", "eth", e1, hop1Out, e0, f2, u, [2]int64{c1, c0}, &fails)
		}
	}

	if fails == 0 {
		t.Logf("CASE SLIP-PARITY USERFLOW PASS: UF1-UF6")
	} else {
		t.Logf("CASE SLIP-PARITY USERFLOW FAIL: %d checks failed", fails)
	}
}
