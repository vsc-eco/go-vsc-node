package pendulum

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

// invariantResidual is f(D) = Ann·(x+y) + D − Ann·D − D³/(4xy), exactly.
// The true D is the root of f; f is decreasing in D around it.
func invariantResidual(x, y, d *big.Int, amp int64) *big.Rat {
	ann := big.NewInt(amp * 2)
	s := new(big.Int).Add(x, y)
	lhs := new(big.Int).Mul(ann, s)
	lhs.Add(lhs, d)
	lhs.Sub(lhs, new(big.Int).Mul(ann, d))
	d3 := new(big.Int).Mul(d, d)
	d3.Mul(d3, d)
	den := new(big.Int).Mul(x, y)
	den.Mul(den, big.NewInt(4))
	r := new(big.Rat).SetFrac(d3, den)
	return new(big.Rat).Sub(new(big.Rat).SetInt(lhs), r)
}

// requireRootNear asserts the exact root of f lies within [v−tol, v+tol]:
// f changes sign (or is zero) on that interval.
func requireRootNear(t *testing.T, name string, f func(*big.Int) *big.Rat, v *big.Int, tol int64) {
	t.Helper()
	lo := new(big.Int).Sub(v, big.NewInt(tol))
	if lo.Sign() <= 0 {
		lo = big.NewInt(1)
	}
	hi := new(big.Int).Add(v, big.NewInt(tol))
	fl, fh := f(lo), f(hi)
	if fl.Sign()*fh.Sign() > 0 {
		t.Fatalf("%s: no root within ±%d of %s (f(lo)=%s f(hi)=%s)", name, tol, v, fl.FloatString(3), fh.FloatString(3))
	}
}

func bi(v int64) *big.Int { return big.NewInt(v) }

func TestStableGetD_RootOfTheInvariant(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	cases := [][3]int64{
		{1_000_000, 1_000_000, 100},
		{1, 1, 1},
		{1, 1_000_000_000, 100},          // 1e9:1 imbalance
		{1_000_000_000_000, 1_000, 2000}, // the other way
		{math.MaxInt64 / 4, math.MaxInt64 / 3, StableMaxAmp},
		{5, 7, 1},
	}
	for i := 0; i < 300; i++ {
		x := rng.Int63n(1<<50) + 1
		y := rng.Int63n(1<<50) + 1
		amp := rng.Int63n(StableMaxAmp) + 1
		cases = append(cases, [3]int64{x, y, amp})
	}
	for _, c := range cases {
		x, y, amp := bi(c[0]), bi(c[1]), c[2]
		d, err := StableGetD(x, y, amp)
		if err != nil {
			t.Fatalf("GetD(%d,%d,A=%d): %v", c[0], c[1], amp, err)
		}
		// Flooring inside D_P costs ~1e-9 relative at a 1e9:1 imbalance on
		// small numbers; production normalizes to 18 decimals (below).
		tol := 2 + new(big.Int).Quo(d, bi(100_000_000)).Int64()
		requireRootNear(t, "GetD", func(v *big.Int) *big.Rat { return invariantResidual(x, y, v, amp) }, d, tol)
		// D lies between the constant-product value 2·sqrt(xy) and the sum.
		s := new(big.Int).Add(x, y)
		if d.Cmp(new(big.Int).Add(s, bigOne)) > 0 {
			t.Fatalf("GetD(%d,%d,A=%d) = %s > x+y", c[0], c[1], amp, d)
		}
		gm := new(big.Int).Sqrt(new(big.Int).Mul(x, y))
		gm.Mul(gm, bigTwo)
		if d.Cmp(new(big.Int).Sub(gm, bigTwo)) < 0 {
			t.Fatalf("GetD(%d,%d,A=%d) = %s < 2·sqrt(xy) = %s", c[0], c[1], amp, d, gm)
		}
	}
}

func TestStableGetY_RootOfTheLevelSet(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 300; i++ {
		x := bi(rng.Int63n(1<<48) + 1000)
		y := bi(rng.Int63n(1<<48) + 1000)
		amp := rng.Int63n(StableMaxAmp) + 1
		d, err := StableGetD(x, y, amp)
		if err != nil {
			t.Fatal(err)
		}
		// Move x anywhere from a tenth to ten times its size.
		xNew := new(big.Int).Mul(x, bi(rng.Int63n(100)+1))
		xNew.Quo(xNew, bi(10))
		if xNew.Sign() == 0 {
			xNew = bi(1)
		}
		yNew, err := StableGetY(xNew, d, amp)
		if err != nil {
			t.Fatalf("GetY: %v", err)
		}
		// yNew is (up to rounding) the y that keeps D at xNew.
		requireRootNear(t, "GetY", func(v *big.Int) *big.Rat { return invariantResidual(xNew, v, d, amp) }, yNew, 3)
	}
	// Unchanged x gives back (about) the same y.
	x, y := bi(123_456_789), bi(987_654_321)
	d, _ := StableGetD(x, y, 100)
	yBack, _ := StableGetY(x, d, 100)
	if diff := new(big.Int).Sub(yBack, y); diff.CmpAbs(bi(3)) > 0 {
		t.Fatalf("GetY(x, D(x,y)) = %s, want ~%s", yBack, y)
	}
}

func TestStableGetY_MonotoneInX(t *testing.T) {
	x, y := bi(10_000_000), bi(10_000_000)
	d, _ := StableGetD(x, y, 100)
	prev, _ := StableGetY(bi(1_000_000), d, 100)
	for xi := int64(2_000_000); xi <= 50_000_000; xi += 1_000_000 {
		cur, err := StableGetY(bi(xi), d, 100)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Cmp(prev) > 0 {
			t.Fatalf("GetY not decreasing in x at %d: %s > %s", xi, cur, prev)
		}
		prev = cur
	}
}

func TestStableMarginalPrice_MatchesTheCurve(t *testing.T) {
	e9 := bi(1_000_000_000)
	for _, c := range [][3]int64{
		{1_000_000_000, 1_000_000_000, 100},
		{1_500_000_000, 500_000_000, 100},
		{300_000_000, 2_700_000_000, 10},
		{1_000_000_000, 1_000_000_000, 1},
	} {
		// 18-decimal scale, as production: rounding in GetY is negligible
		// against the step.
		x, y, amp := new(big.Int).Mul(bi(c[0]), e9), new(big.Int).Mul(bi(c[1]), e9), c[2]
		d, _ := StableGetD(x, y, amp)
		num, den, err := StableMarginalPrice(x, y, d, amp)
		if err != nil {
			t.Fatal(err)
		}
		p := new(big.Rat).SetFrac(num, den)
		// Numeric slope of the curve over a small step either side.
		h := new(big.Int).Quo(x, bi(1_000_000))
		yl, _ := StableGetY(new(big.Int).Sub(x, h), d, amp)
		yr, _ := StableGetY(new(big.Int).Add(x, h), d, amp)
		slope := new(big.Rat).SetFrac(new(big.Int).Sub(yl, yr), new(big.Int).Mul(h, bigTwo))
		pf, _ := p.Float64()
		sf, _ := slope.Float64()
		if math.Abs(pf-sf)/sf > 1e-4 {
			t.Fatalf("price %v vs curve slope %v at (%d,%d,A=%d)", pf, sf, c[0], c[1], amp)
		}
	}
	// Balanced pool: exactly par.
	d, _ := StableGetD(bi(5_000), bi(5_000), 50)
	num, den, _ := StableMarginalPrice(bi(5_000), bi(5_000), d, 50)
	if num.Cmp(den) != 0 {
		t.Fatalf("balanced pool price %s/%s, want 1", num, den)
	}
}

// The slip rule reproduces THORChain's CLP fee x²Y/(x+X)² on a constant-
// product pool (p0 = Y/X, grossOut = xY/(x+X)), up to integer rounding.
func TestSlipFee_IsTheCLPFeeOnAConstantProductPool(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 500; i++ {
		X := bi(rng.Int63n(1<<40) + 1_000)
		Y := bi(rng.Int63n(1<<40) + 1_000)
		x := new(big.Int).Quo(X, bi(rng.Int63n(50)+2))
		if x.Sign() == 0 {
			continue
		}
		gross := new(big.Int).Mul(x, Y)
		gross.Quo(gross, new(big.Int).Add(x, X))
		got := SlipFee(gross, x, Y, X)
		want := CLPFeeInt(x, X, Y)
		if diff := new(big.Int).Sub(got, want); diff.CmpAbs(bi(2)) > 0 {
			t.Fatalf("x=%s X=%s Y=%s: slip fee %s vs CLP %s", x, X, Y, got, want)
		}
	}
}

func TestSlipFee_Bounds(t *testing.T) {
	if f := SlipFee(bi(0), bi(5), bi(1), bi(1)); f.Sign() != 0 {
		t.Fatal("zero gross must cost nothing")
	}
	// Paid above the spot price (rounding): never negative.
	if f := SlipFee(bi(110), bi(100), bi(1), bi(1)); f.Sign() != 0 {
		t.Fatalf("output above spot must clamp to 0, got %s", f)
	}
	if f := SlipFee(bi(50), bi(100), bi(1), bi(1)); f.Cmp(bi(25)) != 0 {
		t.Fatalf("half the spot value out: fee 50·0.5 = 25, got %s", f)
	}
}

func TestStable_RefusesBadInput(t *testing.T) {
	if _, err := StableGetD(bi(0), bi(5), 100); err != ErrStableInput {
		t.Fatalf("zero reserve: %v", err)
	}
	if _, err := StableGetD(bi(5), bi(5), 0); err != ErrStableInput {
		t.Fatalf("amp 0: %v", err)
	}
	if _, err := StableGetD(bi(5), bi(5), StableMaxAmp+1); err != ErrStableInput {
		t.Fatalf("amp above max: %v", err)
	}
	if _, err := StableGetY(bi(0), bi(5), 100); err != ErrStableInput {
		t.Fatalf("GetY zero x: %v", err)
	}
	if _, err := StableRate(19); err != ErrStableInput {
		t.Fatalf("19 decimals: %v", err)
	}
	r, err := StableRate(3)
	if err != nil || r.Cmp(new(big.Int).Exp(bi(10), bi(15), nil)) != 0 {
		t.Fatalf("HBD rate: %v %v", r, err)
	}
	r, err = StableRate(18)
	if err != nil || r.Cmp(bi(1)) != 0 {
		t.Fatalf("18-decimal rate: %v %v", r, err)
	}
}

// A swap on a balanced, deep stable pool loses far less than the same swap
// on a constant-product pool (the reason for the curve).
func TestStable_BeatsConstantProductNearPeg(t *testing.T) {
	X, Y := bi(1_000_000_000_000), bi(1_000_000_000_000)
	x := bi(100_000_000_000) // 10% of the reserve
	d, _ := StableGetD(X, Y, 100)
	yNew, _ := StableGetY(new(big.Int).Add(X, x), d, 100)
	stableOut := new(big.Int).Sub(Y, yNew)
	cpmmOut := new(big.Int).Quo(new(big.Int).Mul(x, Y), new(big.Int).Add(X, x))
	// stable loses < 0.1%, constant product ~9%
	if new(big.Int).Mul(stableOut, bi(1000)).Cmp(new(big.Int).Mul(x, bi(999))) < 0 {
		t.Fatalf("stable out %s for %s in: more than 0.1%% slip", stableOut, x)
	}
	if cpmmOut.Cmp(new(big.Int).Mul(x, bi(92)).Quo(new(big.Int).Mul(x, bi(92)), bi(100))) > 0 {
		t.Fatalf("cpmm out %s unexpectedly high", cpmmOut)
	}
}

func TestStableFloorY_IsTheCurveAtTheNewInput(t *testing.T) {
	rX, rY := bi(1000), bi(1) // HBD in (3 dec), USDC out (6 dec)
	X, Y := bi(1_000_000_000), bi(1_000_000_000_000)
	x := bi(50_000_000)
	floor, err := StableFloorY(new(big.Int).Add(X, x), X, Y, rX, rY, 100)
	if err != nil {
		t.Fatal(err)
	}
	if floor.Cmp(new(big.Int).Mul(Y, rY)) >= 0 {
		t.Fatal("floor must be below the old reserve after an input")
	}
	// Same input reserve: the floor is the old reserve (no free output).
	same, _ := StableFloorY(X, X, Y, rX, rY, 100)
	if diff := new(big.Int).Sub(same, new(big.Int).Mul(Y, rY)); diff.CmpAbs(bi(3)) > 0 {
		t.Fatalf("floor at unchanged input %s, want ~%s", same, Y)
	}
}

// At production scale (18-decimal normalization) D is exact to the Newton
// stop even at a 1e6:1 imbalance.
func TestStableGetD_ExactAtProductionScale(t *testing.T) {
	hbd, _ := StableRate(3)
	usdc, _ := StableRate(6)
	for _, c := range [][3]int64{
		{1_000, 1_000_000, 100},                 // 1 HBD, 1 USDC
		{1_000_000_000, 1_000_000_000_000, 100}, // 1M each
		{1, 1_000_000_000_000, 100},             // 0.001 HBD vs 1M USDC (1e9:1)
		{1_000_000_000_000, 1_000, 2000},        // 1e9 HBD vs 0.001 USDC (1e12:1)
	} {
		x := new(big.Int).Mul(bi(c[0]), hbd)
		y := new(big.Int).Mul(bi(c[1]), usdc)
		d, err := StableGetD(x, y, c[2])
		if err != nil {
			t.Fatal(err)
		}
		requireRootNear(t, "GetD@1e18", func(v *big.Int) *big.Rat { return invariantResidual(x, y, v, c[2]) }, d, 2)
	}
}

// Newton settles at any imbalance up to 1e18:1 for every A. Splitting the
// D³/(4xy) division in two (Curve's uint256 workaround) made the integer
// iteration fall into a ±9 2-cycle at 1e5:1 with A=1, so a swap on a
// lopsided pool would have been refused for no reason.
func TestStableGetD_ConvergesAtAnyImbalance(t *testing.T) {
	e18 := new(big.Int).Exp(bi(10), bi(18), nil)
	for _, amp := range []int64{1, 10, 100, 1000, 2000, StableMaxAmp} {
		for k := int64(0); k <= 18; k++ {
			big1 := new(big.Int).Mul(e18, new(big.Int).Exp(bi(10), bi(k), nil))
			for _, xy := range [][2]*big.Int{{big1, e18}, {e18, big1}} {
				d, err := StableGetD(xy[0], xy[1], amp)
				if err != nil {
					t.Fatalf("A=%d 1e%d:1: %v", amp, k, err)
				}
				requireRootNear(t, "GetD", func(v *big.Int) *big.Rat { return invariantResidual(xy[0], xy[1], v, amp) }, d, 2)
				y, err := StableGetY(xy[0], d, amp)
				if err != nil {
					t.Fatalf("GetY A=%d 1e%d:1: %v", amp, k, err)
				}
				requireRootNear(t, "GetY", func(v *big.Int) *big.Rat { return invariantResidual(xy[0], v, d, amp) }, y, 2)
			}
		}
	}
}
