package pendulum

import (
	"math"
	"math/big"
	"testing"
)

func TestCLPFeeInt_BalancedPool(t *testing.T) {
	X := biTest("1000000")
	Y := biTest("1000000")
	x := biTest("100")
	got := CLPFeeInt(x, X, Y)
	wantFloat := (100.0 * 100.0 * 1e6) / (1000100.0 * 1000100.0)
	wantInt := int64(math.Floor(wantFloat))
	if got.Int64() != wantInt {
		t.Fatalf("CLPFeeInt got %s want %d (float %.6f)", got, wantInt, wantFloat)
	}
}

func TestCLPFeeInt_MatchesFloatOnGrid(t *testing.T) {
	cases := []struct {
		x, X, Y int64
	}{
		{100, 1_000_000, 1_000_000},
		{1, 1_000, 1_000},
		{50_000, 1_000_000, 2_000_000},
		{1_000_000, 1_000_000, 1_000_000},
		{1, 1, 1},
	}
	for _, tc := range cases {
		got := CLPFeeInt(big.NewInt(tc.x), big.NewInt(tc.X), big.NewInt(tc.Y))
		want := int64(math.Floor(CLPFee(float64(tc.x), float64(tc.X), float64(tc.Y))))
		diff := got.Int64() - want
		if diff < -1 || diff > 1 {
			t.Errorf("x=%d X=%d Y=%d: int=%s float-floor=%d", tc.x, tc.X, tc.Y, got, want)
		}
	}
}

func TestCLPFeeInt_ZeroOrNegativeInputs(t *testing.T) {
	zero := big.NewInt(0)
	if got := CLPFeeInt(zero, big.NewInt(1), big.NewInt(1)); got.Sign() != 0 {
		t.Errorf("x=0 want 0 got %s", got)
	}
	if got := CLPFeeInt(big.NewInt(1), zero, big.NewInt(1)); got.Sign() != 0 {
		t.Errorf("X=0 want 0 got %s", got)
	}
	if got := CLPFeeInt(big.NewInt(1), big.NewInt(1), zero); got.Sign() != 0 {
		t.Errorf("Y=0 want 0 got %s", got)
	}
	if got := CLPFeeInt(nil, big.NewInt(1), big.NewInt(1)); got.Sign() != 0 {
		t.Errorf("nil x want 0 got %s", got)
	}
	if got := CLPFeeInt(big.NewInt(-1), big.NewInt(1), big.NewInt(1)); got.Sign() != 0 {
		t.Errorf("negative x want 0 got %s", got)
	}
}

func TestStabilizerMultiplierBps_AtEquilibrium(t *testing.T) {
	// At s = 0.5 (5000 bps), |s - 0.5| = 0, so m == 1 (10000 bps) regardless of r.
	p := DefaultStabilizerParamsBps()
	got := StabilizerMultiplierBps(5_000, 500, p)
	if got != BpsScale {
		t.Errorf("got %d want %d", got, BpsScale)
	}
}

func TestStabilizerMultiplierBps_MatchesFloatOnGrid(t *testing.T) {
	pFloat := DefaultStabilizerParams()
	pBps := DefaultStabilizerParamsBps()
	for _, s := range []float64{0.1, 0.3, 0.5, 0.7, 0.9} {
		for _, r := range []float64{0, 0.001, 0.01, 0.05, 0.5} {
			fl := StabilizerMultiplier(s, r, pFloat)
			bps := StabilizerMultiplierBps(bpsFromFloat(s), bpsFromFloat(r), pBps)
			fxFloat := float64(bps) / float64(BpsScale)
			// 1 bps == 0.0001; allow ~3 bps drift from chained MulDivFloor rounding.
			if math.Abs(fl-fxFloat) > 3e-4 {
				t.Errorf("s=%v r=%v float=%v bps=%v(=%v)", s, r, fl, bps, fxFloat)
			}
		}
	}
}

func TestStabilizerMultiplierBps_CapEnforced(t *testing.T) {
	p := DefaultStabilizerParamsBps()
	// Force a huge raw multiplier with extreme |s-0.5| and r/r0.
	got := StabilizerMultiplierBps(bpsFromFloat(0.99), bpsFromFloat(10.0), p)
	if got != p.CapBps {
		t.Errorf("got %d want cap %d", got, p.CapBps)
	}
}

func TestApplyMultiplierBps(t *testing.T) {
	// 1000 base units · 1.5 = 1500
	fee := big.NewInt(1000)
	got := ApplyMultiplierBps(fee, 15_000) // 1.5
	if got.Int64() != 1500 {
		t.Fatalf("got %s want 1500", got)
	}
	// Conservation under m=1.
	got = ApplyMultiplierBps(fee, BpsScale)
	if got.Int64() != 1000 {
		t.Fatalf("got %s want 1000", got)
	}
	// Floor when m·fee not integer (1000 · 1.0001 = 1000.1 → 1000).
	got = ApplyMultiplierBps(fee, 10_001)
	if got.Int64() != 1000 {
		t.Fatalf("got %s want 1000 (floor)", got)
	}
}
