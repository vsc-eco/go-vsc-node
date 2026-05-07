package pendulum

import (
	"math/big"
	"testing"
)

func TestCLPFeeInt_BalancedPool(t *testing.T) {
	// CLP = x²·Y / (x+X)² = 100²·1e6 / 1_000_100² = floor(0.00999800...) = 0
	X := biTest("1000000")
	Y := biTest("1000000")
	x := biTest("100")
	got := CLPFeeInt(x, X, Y)
	if got.Int64() != 0 {
		t.Fatalf("CLPFeeInt got %s want 0", got)
	}
}

func TestCLPFeeInt_OnGrid(t *testing.T) {
	// Hand-computed floor(x²·Y / (x+X)²) for each row.
	cases := []struct {
		x, X, Y int64
		want    int64
	}{
		{1, 1_000, 1_000, 0},                       // 1·1000 / 1_002_001 = 0
		{50_000, 1_000_000, 2_000_000, 4535},       // 2.5e9·2e6 / 1.1025e12 = 4535
		{1_000_000, 1_000_000, 1_000_000, 250_000}, // 1e12·1e6 / 4e12 = 250_000
		{1, 1, 1, 0},                               // 1·1 / 4 = 0
	}
	for _, tc := range cases {
		got := CLPFeeInt(big.NewInt(tc.x), big.NewInt(tc.X), big.NewInt(tc.Y))
		if got.Int64() != tc.want {
			t.Errorf("x=%d X=%d Y=%d: got %s want %d", tc.x, tc.X, tc.Y, got, tc.want)
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

func TestStabilizerMultiplierBps_OnGrid(t *testing.T) {
	// m(s, r) = 1 + K·|s−0.5|·(1 + r/R0)·push, capped at Cap.
	// Defaults: K=1.0, R0=0.01, Cap=2.0, Push=1.0.
	p := DefaultStabilizerParamsBps()
	cases := []struct {
		sBps, rBps int64
		want       int64
	}{
		{1_000, 0, 14_000},   // s=0.1, r=0    → 1 + 0.4·1·1 = 1.4
		{3_000, 0, 12_000},   // s=0.3, r=0    → 1 + 0.2·1·1 = 1.2
		{5_000, 5_000, 10_000}, // s=0.5, any r → m == 1 (no deviation)
		{7_000, 100, 14_000}, // s=0.7, r=0.01 → 1 + 0.2·2·1 = 1.4
	}
	for _, c := range cases {
		got := StabilizerMultiplierBps(c.sBps, c.rBps, p)
		if got != c.want {
			t.Errorf("sBps=%d rBps=%d: got %d want %d", c.sBps, c.rBps, got, c.want)
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
