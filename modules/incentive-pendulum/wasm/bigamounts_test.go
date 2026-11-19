package pendulumwasm

import (
	"math/big"
	"math/rand"
	"strings"
	"testing"

	"vsc-node/modules/common/consensusversion"
	wasm_context "vsc-node/modules/wasm/context"
)

func toBig(a wasm_context.PendulumSwapFeeArgs) wasm_context.PendulumSwapFeeArgs {
	a.XBig, a.XReserveBig, a.YReserveBig = big.NewInt(a.X), big.NewInt(a.XReserve), big.NewInt(a.YReserve)
	a.X, a.XReserve, a.YReserve = 0, 0, 0
	return a
}

// SP-0d: within int64 the big-amount answer is the int64 answer, number for
// number, on both curves and both directions (only the representation moves).
func TestBigAmounts_SameAnswerWithinInt64(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_13_0, true)
	rng := rand.New(rand.NewSource(21))
	for i := 0; i < 600; i++ {
		in := []string{"hbd", "usdc"}[i%2]
		args := stableArgs(in, 1)
		if i%4 >= 2 { // constant product on the other pool
			args = cpmmOnly(args)
		}
		args.XReserve = args.XReserve/10 + rng.Int63n(args.XReserve*3)
		args.YReserve = args.YReserve/10 + rng.Int63n(args.YReserve*3)
		args.X = rng.Int63n(args.XReserve/2) + 1
		id := stablePoolID
		if i%4 >= 2 {
			id = "vsc1Other"
		}
		r64 := a.ApplySwapFees(id, "tx", 100, args, (&recordingAccrual{}).fn)
		rBig := a.ApplySwapFees(id, "tx", 100, toBig(args), (&recordingAccrual{}).fn)
		if r64.IsErr() != rBig.IsErr() {
			t.Fatalf("case %d: int64 err=%v big err=%v", i, r64.IsErr(), rBig.IsErr())
		}
		if r64.IsErr() {
			continue
		}
		o, b := r64.Unwrap(), rBig.Unwrap()
		if b.Big == nil {
			t.Fatalf("case %d: big amounts answered without Big", i)
		}
		if b.Big.UserOutput.Int64() != o.UserOutput || b.Big.NewXReserve.Int64() != o.NewXReserve ||
			b.Big.NewYReserve.Int64() != o.NewYReserve || b.Big.NetworkCreditOutput.Int64() != o.NetworkCreditOutput ||
			b.Big.LpShareOutput.Int64() != o.LpShareOutput || b.Big.NodeShareOutput.Int64() != o.NodeShareOutput ||
			b.NodeBucketCreditedHBD != o.NodeBucketCreditedHBD || b.MultiplierBps != o.MultiplierBps ||
			b.SAfterBps != o.SAfterBps || b.Curve != o.Curve {
			t.Fatalf("case %d: int64 %+v vs big %+v / %+v", i, o, b, *b.Big)
		}
	}
}

// Past int64: a constant-product pool holding 100 ETH (1e20 wei) against
// 100k HBD, and a stable pool of an 18-decimal stablecoin (100k "dai" =
// 1e23 base units) against 100k HBD. Both price; the HBD node share still
// fits the ledger. (A stable curve is only meaningful for a pegged pair,
// which is why the network lists the pool and its pair.)
func TestBigAmounts_EighteenDecimalPoolsPastInt64(t *testing.T) {
	daiListing := StableListing{Amp: 100, Asset0: "dai", Decimals0: 18, Asset1: "hbd", Decimals1: 3}
	cfg := DefaultConfig()
	cfg.StablePoolAt = func(id string, _ uint64) (StableListing, bool) { return daiListing, id == stablePoolID }
	a := New(&stubGeometry{out: balancedGeometry()}, func(uint64) []string { return []string{stablePoolID, "vsc1Other"} },
		func(uint64) consensusversion.Version { return consensusversion.V0_13_0 }, cfg)
	e20, _ := new(big.Int).SetString("100000000000000000000", 10)
	e23, _ := new(big.Int).SetString("100000000000000000000000", 10)
	for _, c := range []struct {
		id, out, curve string
		yRes           *big.Int
		lo             string // least acceptable output for 1,000 HBD
	}{
		{"vsc1Other", "eth", "", e20, "980000000000000000"},             // ~0.99 ETH (1% of the ETH side)
		{stablePoolID, "dai", `"stable"`, e23, "997000000000000000000"}, // ~999.x DAI at par
	} {
		args := wasm_context.PendulumSwapFeeArgs{AssetIn: "hbd", AssetOut: c.out,
			XBig: big.NewInt(1_000_000), XReserveBig: big.NewInt(100_000_000), YReserveBig: c.yRes}
		if c.curve != "" {
			args.Curve, args.Amp, args.DecIn, args.DecOut = c.curve, `"100"`, `"3"`, `"18"`
		}
		res := a.ApplySwapFees(c.id, "tx", 100, args, (&recordingAccrual{}).fn)
		if res.IsErr() {
			t.Fatalf("%s: %v", c.out, res.UnwrapErr())
		}
		out := res.Unwrap()
		lo, _ := new(big.Int).SetString(c.lo, 10)
		if out.Big == nil || out.Big.UserOutput.Cmp(lo) < 0 || out.Big.UserOutput.Cmp(c.yRes) >= 0 {
			t.Fatalf("%s: output %v, want at least %s", c.out, out.Big, c.lo)
		}
		t.Logf("%s curve=%q: 1,000 HBD -> %s base units, node bucket %d HBD units", c.out, out.Curve, out.Big.UserOutput, out.NodeBucketCreditedHBD)
	}
}

// A trade ratio x/X past int64 basis points (big amounts make it reachable:
// a huge input against a tiny reserve) is refused, never fed to the
// stabilizer as a wrapped number. The pool contract's own cap (x <= X/2)
// keeps honest swaps far from this.
func TestBigAmounts_RatioOverflowRefused(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_13_0, true)
	e30, _ := new(big.Int).SetString("1000000000000000000000000000000", 10)
	args := wasm_context.PendulumSwapFeeArgs{AssetIn: "hbd", AssetOut: "hive",
		XBig: e30, XReserveBig: big.NewInt(1), YReserveBig: e30}
	res := a.ApplySwapFees("vsc1Other", "tx", 100, args, (&recordingAccrual{}).fn)
	if !res.IsErr() || !strings.Contains(res.UnwrapErr().Error(), errArithmeticOverflow.Error()) {
		t.Fatalf("ratio overflow: %v, want %v", res, errArithmeticOverflow)
	}
}
