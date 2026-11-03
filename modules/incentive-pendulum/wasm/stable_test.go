package pendulumwasm

import (
	"math/big"
	"math/rand"
	"strings"
	"testing"

	"vsc-node/modules/common/consensusversion"
	pendulum "vsc-node/modules/incentive-pendulum"
	wasm_context "vsc-node/modules/wasm/context"
)

const stablePoolID = "vsc1StablePool"

// HBD/USDC listing: HBD 3 decimals, USDC 6, A = 100.
var hbdUsdcListing = StableListing{Amp: 100, Asset0: "hbd", Decimals0: 3, Asset1: "usdc", Decimals1: 6}

func stableApplier(t *testing.T, version consensusversion.Version, listed bool) (*Applier, *recordingAccrual) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.StablePoolAt = func(id string, _ uint64) (StableListing, bool) {
		if listed && id == stablePoolID {
			return hbdUsdcListing, true
		}
		return StableListing{}, false
	}
	a := New(
		&stubGeometry{out: balancedGeometry()},
		func(uint64) []string { return []string{stablePoolID, "vsc1Other"} },
		func(uint64) consensusversion.Version { return version },
		cfg,
	)
	return a, &recordingAccrual{}
}

// 100,000 HBD (3 dec) against 100,000 USDC (6 dec): balanced by value.
func stableArgs(assetIn string, x int64) wasm_context.PendulumSwapFeeArgs {
	args := wasm_context.PendulumSwapFeeArgs{X: x, Curve: `"stable"`, Amp: `"100"`}
	if assetIn == "hbd" {
		args.AssetIn, args.AssetOut = "hbd", "usdc"
		args.XReserve, args.YReserve = 100_000_000, 100_000_000_000
		args.DecIn, args.DecOut = `"3"`, `"6"`
	} else {
		args.AssetIn, args.AssetOut = "usdc", "hbd"
		args.XReserve, args.YReserve = 100_000_000_000, 100_000_000
		args.DecIn, args.DecOut = `"6"`, `"3"`
	}
	return args
}

func cpmmOnly(a wasm_context.PendulumSwapFeeArgs) wasm_context.PendulumSwapFeeArgs {
	a.Curve, a.Amp, a.DecIn, a.DecOut = "", "", "", ""
	return a
}

func mustOK(t *testing.T, a *Applier, id string, args wasm_context.PendulumSwapFeeArgs) wasm_context.PendulumSwapFeeResult {
	t.Helper()
	acc := &recordingAccrual{}
	res := a.ApplySwapFees(id, "tx", 100, args, acc.fn)
	if res.IsErr() {
		t.Fatalf("swap refused: %v", res.UnwrapErr())
	}
	return res.Unwrap()
}

func mustRefuse(t *testing.T, a *Applier, id string, args wasm_context.PendulumSwapFeeArgs, want error) {
	t.Helper()
	acc := &recordingAccrual{}
	res := a.ApplySwapFees(id, "tx", 100, args, acc.fn)
	if !res.IsErr() {
		t.Fatalf("swap accepted (%+v), want refusal %v", res.Unwrap(), want)
	}
	err := res.UnwrapErr()
	if want != nil && !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("refused with %v, want %v", err, want)
	}
	if len(acc.calls) != 0 {
		t.Fatalf("refused swap accrued %v", acc.calls)
	}
}

// Below 0.13.0 nothing changes: a listed pool that sends the stable keys gets
// the constant product, byte for byte what a binary without this code gives.
func TestStable_InertBelow013(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_12_0, true)
	for _, in := range []string{"hbd", "usdc"} {
		got := mustOK(t, a, stablePoolID, stableArgs(in, 5_000_000))
		want := mustOK(t, a, stablePoolID, cpmmOnly(stableArgs(in, 5_000_000)))
		if got != want {
			t.Fatalf("%s in: below 0.13 the stable keys changed the result:\n%+v\n%+v", in, got, want)
		}
		if got.Curve != "" {
			t.Fatalf("curve echoed below 0.13: %q", got.Curve)
		}
	}
	// Garbage in the new keys is ignored too (an older binary never reads them).
	bad := stableArgs("hbd", 5_000)
	bad.Curve, bad.Amp = `{"x":1}`, `[1,2]`
	mustOK(t, a, stablePoolID, bad)
}

// From 0.13.0 an unlisted pool that asks for nothing is the constant product
// exactly as before.
func TestStable_ConstantProductPoolsUnchangedAt013(t *testing.T) {
	a013, _ := stableApplier(t, consensusversion.V0_13_0, true)
	a012, _ := stableApplier(t, consensusversion.V0_12_0, true)
	for _, in := range []string{"hbd", "usdc"} {
		args := cpmmOnly(stableArgs(in, 7_000_000))
		if got, want := mustOK(t, a013, "vsc1Other", args), mustOK(t, a012, "vsc1Other", args); got != want {
			t.Fatalf("%s in: 0.13 changed a constant-product pool:\n%+v\n%+v", in, got, want)
		}
	}
}

func TestStable_PricesOnTheCurve(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_13_0, true)
	for _, in := range []string{"hbd", "usdc"} {
		// 10% of the reserve.
		x := int64(10_000_000)
		if in == "usdc" {
			x = 10_000_000_000
		}
		st := mustOK(t, a, stablePoolID, stableArgs(in, x))
		cp := mustOK(t, a, "vsc1Other", cpmmOnly(stableArgs(in, x)))
		if st.Curve != "stable" {
			t.Fatalf("%s in: curve %q", in, st.Curve)
		}
		// Par value of x in the output asset.
		par := big.NewInt(x)
		if in == "hbd" {
			par.Mul(par, big.NewInt(1000))
		} else {
			par.Quo(par, big.NewInt(1000))
		}
		// Stable: within 0.3% of par (fees ~8-16 bps + tiny slip).
		if new(big.Int).Mul(big.NewInt(st.UserOutput), big.NewInt(1000)).Cmp(new(big.Int).Mul(par, big.NewInt(997))) < 0 {
			t.Fatalf("%s in: stable user output %d for par %s", in, st.UserOutput, par)
		}
		if st.UserOutput <= cp.UserOutput {
			t.Fatalf("%s in: stable %d not above constant product %d", in, st.UserOutput, cp.UserOutput)
		}
	}
}

// Every unit is accounted for, both directions: what leaves the output
// reserve is the user's output plus (HBD out) the node share; what leaves
// the HBD reserve on the USDC-out side is exactly the node bucket.
func TestStable_Conservation(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_13_0, true)
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 400; i++ {
		in := "hbd"
		if i%2 == 1 {
			in = "usdc"
		}
		args := stableArgs(in, 1)
		args.XReserve = args.XReserve/10 + rng.Int63n(args.XReserve*3)
		args.YReserve = args.YReserve/10 + rng.Int63n(args.YReserve*3)
		args.X = rng.Int63n(args.XReserve/2) + 1
		acc := &recordingAccrual{}
		res := a.ApplySwapFees(stablePoolID, "tx", 100, args, acc.fn)
		if res.IsErr() {
			continue // dust that rounds to nothing
		}
		out := res.Unwrap()
		var bucket int64
		for _, c := range acc.calls {
			bucket += c
		}
		if bucket != out.NodeBucketCreditedHBD {
			t.Fatalf("bucket %d vs reported %d", bucket, out.NodeBucketCreditedHBD)
		}
		fees := out.LpShareOutput + out.NodeShareOutput + out.NetworkCreditOutput
		if in == "hbd" { // USDC out: node share sold back for HBD
			if args.YReserve-out.NewYReserve != out.UserOutput {
				t.Fatalf("usdc out: reserve drop %d != user %d", args.YReserve-out.NewYReserve, out.UserOutput)
			}
			if args.XReserve+args.X-out.NewXReserve != out.NodeBucketCreditedHBD {
				t.Fatalf("usdc out: hbd drop %d != bucket %d", args.XReserve+args.X-out.NewXReserve, out.NodeBucketCreditedHBD)
			}
		} else { // HBD out: node share leaves Y as HBD
			if args.YReserve-out.NewYReserve != out.UserOutput+out.NodeShareOutput {
				t.Fatalf("hbd out: reserve drop %d != user %d + node %d", args.YReserve-out.NewYReserve, out.UserOutput, out.NodeShareOutput)
			}
			if out.NodeBucketCreditedHBD != out.NodeShareOutput {
				t.Fatalf("hbd out: bucket %d != node share %d", out.NodeBucketCreditedHBD, out.NodeShareOutput)
			}
			if out.NewXReserve != args.XReserve+args.X {
				t.Fatalf("hbd out: input reserve %d != %d", out.NewXReserve, args.XReserve+args.X)
			}
		}
		if fees < 0 || out.UserOutput <= 0 {
			t.Fatalf("negative fee legs or output: %+v", out)
		}
	}
}

// The pool contract's check (stored reserves on or above the old level set)
// passes for every swap the node prices, on balanced and lopsided pools.
func TestStable_EverySwapPassesTheContractFloor(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_13_0, true)
	rng := rand.New(rand.NewSource(9))
	checked := 0
	for i := 0; i < 1000; i++ {
		in := "hbd"
		if i%2 == 1 {
			in = "usdc"
		}
		args := stableArgs(in, 1)
		// up to 30:1 either way
		args.XReserve = args.XReserve/30 + rng.Int63n(args.XReserve*30)
		args.YReserve = args.YReserve/30 + rng.Int63n(args.YReserve*30)
		args.X = rng.Int63n(args.XReserve/2) + 1
		acc := &recordingAccrual{}
		res := a.ApplySwapFees(stablePoolID, "tx", 100, args, acc.fn)
		if res.IsErr() {
			continue
		}
		out := res.Unwrap()
		decIn, decOut := uint8(3), uint8(6)
		if in == "usdc" {
			decIn, decOut = 6, 3
		}
		rIn, _ := pendulum.StableRate(decIn)
		rOut, _ := pendulum.StableRate(decOut)
		storedX := big.NewInt(out.NewXReserve)
		storedY := big.NewInt(out.NewYReserve - out.NetworkCreditOutput)
		floor, err := pendulum.StableFloorY(storedX, big.NewInt(args.XReserve), big.NewInt(args.YReserve), rIn, rOut, 100)
		if err != nil {
			t.Fatal(err)
		}
		if new(big.Int).Mul(storedY, rOut).Cmp(floor) < 0 {
			t.Fatalf("case %d (%s in, X=%d Y=%d x=%d): stored %s·%s below floor %s", i, in, args.XReserve, args.YReserve, args.X, storedY, rOut, floor)
		}
		checked++
	}
	if checked < 900 {
		t.Fatalf("only %d of 1000 swaps priced", checked)
	}
}

// The USDC node share is sold back on the curve, at the curve's spot price
// of the pool after the swap. On a pool holding 3x more HBD than USDC by
// value the constant-product hop (reserve ratio) would have paid nodes
// several times the HBD.
func TestStable_NodeShareSoldBackOnTheCurve(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_13_0, true)
	args := stableArgs("hbd", 20_000_000)
	args.XReserve, args.YReserve = 150_000_000, 50_000_000_000 // 150k HBD, 50k USDC
	out := mustOK(t, a, stablePoolID, args)
	if out.NodeShareOutput <= 0 {
		t.Fatalf("no node share to sell: %+v", out)
	}
	rHBD, _ := pendulum.StableRate(3)
	rUSDC, _ := pendulum.StableRate(6)
	// LP reserve right before the sell-back.
	xAfter := big.NewInt(args.XReserve + args.X)
	yLP := big.NewInt(args.YReserve - out.UserOutput - out.NodeShareOutput - out.NetworkCreditOutput)
	xp, yp := new(big.Int).Mul(xAfter, rHBD), new(big.Int).Mul(yLP, rUSDC)
	d, _ := pendulum.StableGetD(xp, yp, 100)
	// price of one normalized USDC in normalized HBD
	num, den, _ := pendulum.StableMarginalPrice(yp, xp, d, 100)
	spot := new(big.Rat).SetFrac(num, den)
	want := new(big.Rat).Mul(spot, new(big.Rat).SetFrac64(out.NodeShareOutput, 1000)) // USDC units -> HBD units at par, times price
	got := new(big.Rat).SetInt64(out.NodeBucketCreditedHBD)
	ratio, _ := new(big.Rat).Quo(got, want).Float64()
	if ratio > 1.0 || ratio < 0.99 {
		t.Fatalf("node bucket %d HBD vs spot value %s (ratio %.4f)", out.NodeBucketCreditedHBD, want.FloatString(1), ratio)
	}
	// Exactly: sold on the LP reserve (network credit excluded: the contract
	// moves it to its fee bucket), on D of that reserve, floored, less one.
	xNew, _ := pendulum.StableGetY(new(big.Int).Add(yp, new(big.Int).Mul(big.NewInt(out.NodeShareOutput), rUSDC)), d, 100)
	exact := new(big.Int).Sub(xp, xNew)
	exact.Quo(exact, rHBD)
	exact.Sub(exact, big.NewInt(1))
	if exact.Int64() != out.NodeBucketCreditedHBD {
		t.Fatalf("node bucket %d, want %s (sold on the LP reserve)", out.NodeBucketCreditedHBD, exact)
	}
	cpmmHop := new(big.Int).Mul(big.NewInt(out.NodeShareOutput), xAfter)
	cpmmHop.Quo(cpmmHop, new(big.Int).Add(yLP, big.NewInt(out.NodeShareOutput)))
	if cpmmHop.Cmp(new(big.Int).Mul(big.NewInt(out.NodeBucketCreditedHBD), big.NewInt(3))) < 0 {
		t.Fatalf("constant-product hop %s not far above the curve's %d: test pool not lopsided enough", cpmmHop, out.NodeBucketCreditedHBD)
	}
}

func TestStable_Refusals(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_13_0, true)
	unlisted, _ := stableApplier(t, consensusversion.V0_13_0, false)

	mustRefuse(t, unlisted, stablePoolID, stableArgs("hbd", 5_000), errStableUnlisted)
	mustRefuse(t, a, stablePoolID, cpmmOnly(stableArgs("hbd", 5_000)), errStableNotAsked)

	bad := stableArgs("hbd", 5_000)
	bad.Amp = `"200"`
	mustRefuse(t, a, stablePoolID, bad, errStableMismatch)

	bad = stableArgs("hbd", 5_000)
	bad.DecIn, bad.DecOut = `"6"`, `"3"` // decimals swapped
	mustRefuse(t, a, stablePoolID, bad, errStableMismatch)

	bad = stableArgs("hbd", 5_000)
	bad.AssetOut = "hive"
	mustRefuse(t, a, stablePoolID, bad, errStableMismatch)

	bad = stableArgs("hbd", 5_000)
	bad.Curve = `"curve-v2"`
	mustRefuse(t, a, stablePoolID, bad, errStableCurve)

	bad = stableArgs("hbd", 5_000)
	bad.Curve = `7`
	mustRefuse(t, a, stablePoolID, bad, errStableCurve)

	bad = stableArgs("hbd", 5_000)
	bad.Amp = ``
	mustRefuse(t, a, stablePoolID, bad, errStableMismatch)

	// Numbers instead of strings are fine.
	ok := stableArgs("usdc", 5_000_000)
	ok.Amp, ok.DecIn, ok.DecOut = `100`, `6`, `3`
	if out := mustOK(t, a, stablePoolID, ok); out.Curve != "stable" {
		t.Fatalf("numeric parameters: curve %q", out.Curve)
	}
}

func TestStable_RawValueParsers(t *testing.T) {
	for raw, want := range map[string]int64{`"100"`: 100, `100`: 100, ` 7 `: 7} {
		if v, ok := rawInt(raw); !ok || v != want {
			t.Fatalf("rawInt(%q) = %d %v", raw, v, ok)
		}
	}
	for _, raw := range []string{``, `"x"`, `1.5`, `"1e3"`, `[1]`, `"`} {
		if _, ok := rawInt(raw); ok {
			t.Fatalf("rawInt(%q) accepted", raw)
		}
	}
	if s, ok := rawString(``); !ok || s != "" {
		t.Fatal("absent curve")
	}
	if _, ok := rawString(`5`); ok {
		t.Fatal("number as curve")
	}
}

// The gross output is the curve's, rounded down one unit in the pool's favour
// (Curve: dy = xp[j] - y - 1), in both directions, every pool shape and size:
// gross = (yp - getY(xp + dx)) / rate - 1, read back from the result through
// its reconciliation (user + LP share + node share + network credit).
func TestStable_GrossIsTheCurveRoundedDown(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_13_0, true)
	rHBD, _ := pendulum.StableRate(3)
	rUSDC, _ := pendulum.StableRate(6)
	n := 0
	for _, in := range []string{"hbd", "usdc"} {
		for _, shape := range [][2]int64{{100_000_000, 100_000_000_000}, {150_000_000, 50_000_000_000}, {20_000_000, 400_000_000_000}} {
			for _, pct := range []int64{1, 100, 1_000, 10_000, 40_000} { // per 100,000 of the input reserve
				args := stableArgs(in, 1)
				rIn, rOut := rHBD, rUSDC
				args.XReserve, args.YReserve = shape[0], shape[1]
				if in == "usdc" {
					rIn, rOut = rUSDC, rHBD
					args.XReserve, args.YReserve = shape[1], shape[0]
				}
				args.X = args.XReserve * pct / 100_000
				if args.X == 0 {
					args.X = 1
				}
				res := a.ApplySwapFees(stablePoolID, "tx", 100, args, (&recordingAccrual{}).fn)
				if res.IsErr() {
					continue // a dust swap the applier refuses is not this test's case
				}
				out := res.Unwrap()
				xp := new(big.Int).Mul(big.NewInt(args.XReserve), rIn)
				yp := new(big.Int).Mul(big.NewInt(args.YReserve), rOut)
				d, _ := pendulum.StableGetD(xp, yp, 100)
				yNew, _ := pendulum.StableGetY(new(big.Int).Add(xp, new(big.Int).Mul(big.NewInt(args.X), rIn)), d, 100)
				want := new(big.Int).Sub(yp, yNew)
				want.Quo(want, rOut)
				want.Sub(want, big.NewInt(1))
				got := out.UserOutput + out.LpShareOutput + out.NodeShareOutput + out.NetworkCreditOutput
				if want.Int64() != got {
					t.Fatalf("%s in, reserves %d/%d, x %d: gross %d, want %s", in, args.XReserve, args.YReserve, args.X, got, want)
				}
				n++
			}
		}
	}
	if n < 25 {
		t.Fatalf("only %d of 30 cases priced", n)
	}
}

// The node share's sell-back is priced on the LP reserve (the network credit
// is the contract's, not the LPs'), exactly, over pool shapes from balanced to
// 500:1 and swap sizes up to 40% of the input reserve.
func TestStable_SellBackExactOverShapes(t *testing.T) {
	a, _ := stableApplier(t, consensusversion.V0_13_0, true)
	rHBD, _ := pendulum.StableRate(3)
	rUSDC, _ := pendulum.StableRate(6)
	cases := 0
	for _, shape := range [][2]int64{{100_000_000, 100_000_000_000}, {150_000_000, 50_000_000_000}, {1_000_000_000, 10_000_000_000}, {1_000_000_000, 2_000_000_000}, {10_000_000, 1_000_000_000_000}} {
		for _, pct := range []int64{100, 1_000, 5_000, 20_000, 40_000} {
			args := stableArgs("hbd", 1)
			args.XReserve, args.YReserve = shape[0], shape[1]
			args.X = shape[0] * pct / 100_000
			res := a.ApplySwapFees(stablePoolID, "tx", 100, args, (&recordingAccrual{}).fn)
			if res.IsErr() {
				continue
			}
			out := res.Unwrap()
			if out.NodeShareOutput <= 0 {
				continue
			}
			xAfter := big.NewInt(args.XReserve + args.X)
			yLP := big.NewInt(args.YReserve - out.UserOutput - out.NodeShareOutput - out.NetworkCreditOutput)
			xp, yp := new(big.Int).Mul(xAfter, rHBD), new(big.Int).Mul(yLP, rUSDC)
			d, _ := pendulum.StableGetD(xp, yp, 100)
			xNew, _ := pendulum.StableGetY(new(big.Int).Add(yp, new(big.Int).Mul(big.NewInt(out.NodeShareOutput), rUSDC)), d, 100)
			exact := new(big.Int).Sub(xp, xNew)
			exact.Quo(exact, rHBD)
			exact.Sub(exact, big.NewInt(1))
			if exact.Sign() < 0 {
				exact.SetInt64(0)
			}
			if exact.Int64() != out.NodeBucketCreditedHBD {
				t.Fatalf("reserves %d/%d, x %d: node bucket %d, want %s (sold on the LP reserve)", args.XReserve, args.YReserve, args.X, out.NodeBucketCreditedHBD, exact)
			}
			cases++
		}
	}
	if cases < 15 {
		t.Fatalf("only %d sell-back cases", cases)
	}
}
