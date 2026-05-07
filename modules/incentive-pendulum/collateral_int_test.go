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
	cases := []struct {
		s        int64 // bps
		under    bool
		ideal    bool
		safe     bool
		warn     bool
		extremeL bool
	}{
		{10_000, true, false, false, true, false}, // s=1.0
		{5_000, false, true, true, false, false},  // s=0.5
		{4_500, false, true, true, false, false},
		{5_500, false, true, true, false, false},
		{4_400, false, false, true, false, false},
		{5_600, false, false, true, false, false},
		{7_600, false, false, false, true, false},
		{2_400, false, false, false, true, false},
		{1_900, false, false, false, true, true},
	}
	for _, tc := range cases {
		r := CollateralFromSVBps(tc.s)
		if r.UnderSecured != tc.under || r.IdealZone != tc.ideal || r.SafeGrowth != tc.safe || r.WarningZone != tc.warn || r.ExtremeLow != tc.extremeL {
			t.Errorf("s=%d got %+v", tc.s, r)
		}
	}
}

func TestCollateralFromSVBps_RedirectFlags(t *testing.T) {
	// |s-0.5| > 0.2 triggers redirect. s=0.2 → starved nodes (s<0.3); s=0.8 → starved pools.
	r := CollateralFromSVBps(2_000)
	if !r.ProtocolRedirectRecommended || !r.ProtocolRedirectToNodes {
		t.Errorf("s=0.2: got %+v", r)
	}
	r = CollateralFromSVBps(8_000)
	if !r.ProtocolRedirectRecommended || r.ProtocolRedirectToNodes {
		t.Errorf("s=0.8: got %+v", r)
	}
	r = CollateralFromSVBps(5_000)
	if r.ProtocolRedirectRecommended {
		t.Errorf("s=0.5: redirect should not trigger; got %+v", r)
	}
}

func TestProtocolFeeRedirectBps_MatchesFloat(t *testing.T) {
	for _, s := range []float64{0, 0.1, 0.29, 0.3, 0.31, 0.5, 0.69, 0.7, 0.71, 0.95, 1.0} {
		fl := ProtocolFeeRedirectRecommended(s)
		fx := ProtocolFeeRedirectRecommendedBps(bpsFromFloat(s))
		if fl != fx {
			t.Errorf("s=%v: float=%v bps=%v", s, fl, fx)
		}
	}
}
