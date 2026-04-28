package pendulum

import (
	"math"
	"math/big"
	"testing"
)

func TestCLPFeeInt_BalancedPool(t *testing.T) {
	// Mirror TestCLPFeeBalancedPool in base-unit terms.
	X := biTest("1000000")
	Y := biTest("1000000")
	x := biTest("100")
	got := CLPFeeInt(x, X, Y)
	// x² Y / (x+X)² = 100·100·1e6 / 1000100² = 1e10 / 1.0002e12 ≈ 9.998
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
		{1_000_000, 1_000_000, 1_000_000}, // x = X exactly
		{1, 1, 1},                          // tiny pool
	}
	for _, tc := range cases {
		got := CLPFeeInt(big.NewInt(tc.x), big.NewInt(tc.X), big.NewInt(tc.Y))
		want := int64(math.Floor(CLPFee(float64(tc.x), float64(tc.X), float64(tc.Y))))
		// Allow 1 base-unit drift from float rounding noise (very small numerators).
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

func TestSQ64_RoundTrip(t *testing.T) {
	cases := []float64{0, 0.5, 1.0, 1.5, 0.0008, 0.123456}
	for _, f := range cases {
		q := SQ64FromFloat(f)
		back := q.ToFloat()
		// 8-decimal scale → tolerate 1e-8 drift.
		if math.Abs(back-f) > 1e-8 {
			t.Errorf("round-trip %v → %d → %v", f, int64(q), back)
		}
	}
}

func TestSQ64Mul(t *testing.T) {
	// 0.5 * 0.5 = 0.25
	got := SQ64Mul(SQ64FromFloat(0.5), SQ64FromFloat(0.5))
	want := SQ64FromFloat(0.25)
	if got != want {
		t.Errorf("0.5*0.5 → %d want %d", int64(got), int64(want))
	}
}

func TestSQ64Div(t *testing.T) {
	// 1.0 / 4.0 = 0.25
	got, ok := SQ64Div(SQ64FromFloat(1.0), SQ64FromFloat(4.0))
	if !ok {
		t.Fatal()
	}
	want := SQ64FromFloat(0.25)
	if got != want {
		t.Errorf("1/4 → %d want %d", int64(got), int64(want))
	}
	// /0
	if _, ok := SQ64Div(SQ64FromFloat(1.0), 0); ok {
		t.Error("expected !ok on /0")
	}
}

func TestStabilizerMultiplierFixed_AtEquilibrium(t *testing.T) {
	// At s = 0.5, |s-0.5| = 0, so m == 1 regardless of r.
	p := DefaultStabilizerParamsFixed()
	got := StabilizerMultiplierFixed(SQ64FromFloat(0.5), SQ64FromFloat(0.05), p)
	want := SQ64FromFloat(1.0)
	if got != want {
		t.Errorf("got %d want %d", int64(got), int64(want))
	}
}

func TestStabilizerMultiplierFixed_MatchesFloatOnGrid(t *testing.T) {
	pFloat := DefaultStabilizerParams()
	pFixed := DefaultStabilizerParamsFixed()
	for _, s := range []float64{0.1, 0.3, 0.5, 0.7, 0.9} {
		for _, r := range []float64{0, 0.001, 0.01, 0.05, 0.5} {
			fl := StabilizerMultiplier(s, r, pFloat)
			fx := StabilizerMultiplierFixed(SQ64FromFloat(s), SQ64FromFloat(r), pFixed).ToFloat()
			if math.Abs(fl-fx) > 1e-6 {
				t.Errorf("s=%v r=%v float=%v fixed=%v", s, r, fl, fx)
			}
		}
	}
}

func TestStabilizerMultiplierFixed_CapEnforced(t *testing.T) {
	p := DefaultStabilizerParamsFixed()
	// Force a huge raw multiplier with extreme |s-0.5| and r/r0.
	got := StabilizerMultiplierFixed(SQ64FromFloat(0.99), SQ64FromFloat(10.0), p)
	want := p.Cap
	if got != want {
		t.Errorf("got %d want cap %d", int64(got), int64(want))
	}
}

func TestApplyMultiplierFixed(t *testing.T) {
	// 1000 base units · 1.5 = 1500
	fee := big.NewInt(1000)
	got := ApplyMultiplierFixed(fee, SQ64FromFloat(1.5))
	if got.Int64() != 1500 {
		t.Fatalf("got %s want 1500", got)
	}
	// Conservation under m=1: same fee.
	got = ApplyMultiplierFixed(fee, SQ64FromFloat(1.0))
	if got.Int64() != 1000 {
		t.Fatalf("got %s want 1000", got)
	}
	// Floor when m·fee not integer (1000 · 1.0000001 = 1000.0001 → 1000).
	got = ApplyMultiplierFixed(fee, SQ64FromFloat(1.0000001))
	if got.Int64() != 1000 {
		t.Fatalf("got %s want 1000 (floor)", got)
	}
}
