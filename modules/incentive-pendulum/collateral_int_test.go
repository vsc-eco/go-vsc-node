package pendulum

import (
	"math/big"
	"testing"
)

func bpsFromFloat(f float64) int64 {
	if f >= 0 {
		return int64(f*float64(BpsScale) + 0.5)
	}
	return int64(f*float64(BpsScale) - 0.5)
}

func TestRatioSVBps(t *testing.T) {
	cases := []struct {
		V, E int64
		want int64
	}{
		{500, 1000, 5_000},  // 0.5
		{1000, 1000, 10_000}, // 1.0
		{0, 1000, 0},
		{500, 0, 0}, // E=0 guard
		{2000, 1000, 20_000}, // 2.0
	}
	for _, tc := range cases {
		got := RatioSVBps(big.NewInt(tc.V), big.NewInt(tc.E))
		if got != tc.want {
			t.Errorf("V=%d E=%d: got %d want %d", tc.V, tc.E, got, tc.want)
		}
	}
}

func TestRatioSVBps_NilSafe(t *testing.T) {
	if RatioSVBps(nil, big.NewInt(1)) != 0 {
		t.Error("nil V should give 0")
	}
	if RatioSVBps(big.NewInt(1), nil) != 0 {
		t.Error("nil E should give 0")
	}
}

func TestEffectiveBondHBDInt(t *testing.T) {
	// 1000 HIVE base × 0.3 HBD/HIVE × 2/3 = 200 HBD base
	got := EffectiveBondHBDInt(big.NewInt(1000), 3_000, bpsFromFloat(2.0/3.0))
	v := got.Int64()
	if v != 200 && v != 199 {
		t.Fatalf("got %d want 200 (or 199 from rounding)", v)
	}
}

func TestEffectiveBondHBDInt_ZeroAndGuards(t *testing.T) {
	if v := EffectiveBondHBDInt(big.NewInt(0), BpsScale, BpsScale); v.Sign() != 0 {
		t.Error("stake=0 should give 0")
	}
	if v := EffectiveBondHBDInt(big.NewInt(1000), 0, BpsScale); v.Sign() != 0 {
		t.Error("price=0 should give 0")
	}
	if v := EffectiveBondHBDInt(big.NewInt(1000), BpsScale, 0); v.Sign() != 0 {
		t.Error("fraction=0 should give 0")
	}
	if v := EffectiveBondHBDInt(nil, BpsScale, BpsScale); v.Sign() != 0 {
		t.Error("nil stake should give 0")
	}
}

func TestEffectiveBondHBDInt_FractionClampedToOne(t *testing.T) {
	// fraction > 1.0 (10000 bps) should be clamped to 1.0.
	a := EffectiveBondHBDInt(big.NewInt(1000), 5_000, 20_000)
	b := EffectiveBondHBDInt(big.NewInt(1000), 5_000, 10_000)
	if a.Cmp(b) != 0 {
		t.Errorf("fraction>1 not clamped: got %s vs %s", a, b)
	}
}

func TestCollateralFromSVBps_Bands(t *testing.T) {
	// Bands are the curve-derived yield-ratio level sets (params.go), asymmetric
	// around s_eq = 1.0: extremeLo<3631, warnLo<4601, safe[5600,15427],
	// ideal[8829,11232], warnHi>17061, extremeHi>21097, cliff≥30000.
	cases := []struct {
		s        int64 // bps
		under    bool
		ideal    bool
		safe     bool
		warn     bool
		extremeL bool
		extremeH bool
	}{
		{10_000, false, true, true, false, false, false},  // s=1.0 equilibrium: ideal+safe
		{8_829, false, true, true, false, false, false},   // ideal lower edge
		{11_232, false, true, true, false, false, false},  // ideal upper edge
		{8_828, false, false, true, false, false, false},  // just below ideal → safe only
		{5_600, false, false, true, false, false, false},  // safe lower edge
		{15_427, false, false, true, false, false, false}, // safe upper edge
		{4_600, false, false, false, true, false, false},  // warning low
		{17_062, false, false, false, true, false, false}, // warning high
		{3_630, false, false, false, true, true, false},   // extreme low
		{21_098, false, false, false, true, false, true},  // extreme high
		{30_000, true, false, false, true, false, true},   // cliff: under-secured (also warn + extreme-high)
	}
	for _, tc := range cases {
		r := CollateralFromSVBps(tc.s)
		if r.UnderSecured != tc.under || r.IdealZone != tc.ideal || r.SafeGrowth != tc.safe ||
			r.WarningZone != tc.warn || r.ExtremeLow != tc.extremeL || r.ExtremeHigh != tc.extremeH {
			t.Errorf("s=%d got %+v", tc.s, r)
		}
	}
}

func TestCollateralFromSVBps_RedirectFlags(t *testing.T) {
	// Redirect fires outside the safe band [5600,15427]. Direction is corrected:
	// starved liquidity (s low) → LPs (toNodes=false); excess (s high) → nodes.
	r := CollateralFromSVBps(5_000) // below safe-lo
	if !r.ProtocolRedirectRecommended || r.ProtocolRedirectToNodes {
		t.Errorf("s=0.5 (low): want recommended + toLPs; got %+v", r)
	}
	r = CollateralFromSVBps(20_000) // above safe-hi
	if !r.ProtocolRedirectRecommended || !r.ProtocolRedirectToNodes {
		t.Errorf("s=2.0 (high): want recommended + toNodes; got %+v", r)
	}
	r = CollateralFromSVBps(10_000) // equilibrium, inside safe band
	if r.ProtocolRedirectRecommended {
		t.Errorf("s=1.0: redirect should not trigger; got %+v", r)
	}
}

func TestProtocolFeeRedirectBps_BandTransitions(t *testing.T) {
	// Redirect recommended when s leaves the safe band: s<5600 or s>15427.
	cases := []struct {
		sBps int64
		want bool
	}{
		{0, true},
		{3_000, true},
		{5_599, true},
		{5_600, false}, // safe-lo edge closed
		{5_601, false},
		{10_000, false},
		{15_427, false}, // safe-hi edge closed
		{15_428, true},
		{20_000, true},
		{30_000, true},
	}
	for _, c := range cases {
		got := ProtocolFeeRedirectRecommendedBps(c.sBps)
		if got != c.want {
			t.Errorf("sBps=%d: got %v, want %v", c.sBps, got, c.want)
		}
	}
}
