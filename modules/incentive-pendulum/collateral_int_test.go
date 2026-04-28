package pendulum

import (
	"math/big"
	"testing"
)

func TestRatioSVFixed(t *testing.T) {
	cases := []struct {
		V, E int64
		want SQ64
	}{
		{500, 1000, SQ64FromFloat(0.5)},
		{1000, 1000, SQ64FromFloat(1.0)},
		{0, 1000, 0},
		{500, 0, 0}, // E=0 guard
		{2000, 1000, SQ64FromFloat(2.0)},
	}
	for _, tc := range cases {
		got := RatioSVFixed(big.NewInt(tc.V), big.NewInt(tc.E))
		if got != tc.want {
			t.Errorf("V=%d E=%d: got %d want %d", tc.V, tc.E, int64(got), int64(tc.want))
		}
	}
}

func TestRatioSVFixed_NilSafe(t *testing.T) {
	if RatioSVFixed(nil, big.NewInt(1)) != 0 {
		t.Error("nil V should give 0")
	}
	if RatioSVFixed(big.NewInt(1), nil) != 0 {
		t.Error("nil E should give 0")
	}
}

func TestEffectiveBondHBDInt(t *testing.T) {
	// 1000 HIVE base × 0.3 HBD/HIVE × 2/3 = 200 HBD base
	got := EffectiveBondHBDInt(big.NewInt(1000), SQ64FromFloat(0.3), SQ64FromFloat(2.0/3.0))
	// Floor due to fixed-point rounding; allow exact 200 or 199.
	v := got.Int64()
	if v != 200 && v != 199 {
		t.Fatalf("got %d want 200 (or 199 from rounding)", v)
	}
}

func TestEffectiveBondHBDInt_ZeroAndGuards(t *testing.T) {
	if v := EffectiveBondHBDInt(big.NewInt(0), SQ64FromFloat(1), SQ64FromFloat(1)); v.Sign() != 0 {
		t.Error("stake=0 should give 0")
	}
	if v := EffectiveBondHBDInt(big.NewInt(1000), 0, SQ64FromFloat(1)); v.Sign() != 0 {
		t.Error("price=0 should give 0")
	}
	if v := EffectiveBondHBDInt(big.NewInt(1000), SQ64FromFloat(1), 0); v.Sign() != 0 {
		t.Error("fraction=0 should give 0")
	}
	if v := EffectiveBondHBDInt(nil, SQ64FromFloat(1), SQ64FromFloat(1)); v.Sign() != 0 {
		t.Error("nil stake should give 0")
	}
}

func TestEffectiveBondHBDInt_FractionClampedToOne(t *testing.T) {
	// fraction > 1 should be clamped to 1.
	a := EffectiveBondHBDInt(big.NewInt(1000), SQ64FromFloat(0.5), SQ64FromFloat(2.0))
	b := EffectiveBondHBDInt(big.NewInt(1000), SQ64FromFloat(0.5), SQ64FromFloat(1.0))
	if a.Cmp(b) != 0 {
		t.Errorf("fraction>1 not clamped: got %s vs %s", a, b)
	}
}

func TestCollateralFromSVFixed_Bands(t *testing.T) {
	cases := []struct {
		s        SQ64
		under    bool
		ideal    bool
		safe     bool
		warn     bool
		extremeL bool
	}{
		{SQ64FromFloat(1.0), true, false, false, true, false},
		{SQ64FromFloat(0.5), false, true, true, false, false},
		{SQ64FromFloat(0.45), false, true, true, false, false},
		{SQ64FromFloat(0.55), false, true, true, false, false},
		{SQ64FromFloat(0.44), false, false, true, false, false},
		{SQ64FromFloat(0.56), false, false, true, false, false},
		{SQ64FromFloat(0.76), false, false, false, true, false},
		{SQ64FromFloat(0.24), false, false, false, true, false},
		{SQ64FromFloat(0.19), false, false, false, true, true},
	}
	for _, tc := range cases {
		r := CollateralFromSVFixed(tc.s)
		if r.UnderSecured != tc.under || r.IdealZone != tc.ideal || r.SafeGrowth != tc.safe || r.WarningZone != tc.warn || r.ExtremeLow != tc.extremeL {
			t.Errorf("s=%v got %+v", tc.s.ToFloat(), r)
		}
	}
}

func TestCollateralFromSVFixed_RedirectFlags(t *testing.T) {
	// |s-0.5| > 0.2 triggers redirect. s=0.2 → starved nodes (s<0.3); s=0.8 → starved pools.
	r := CollateralFromSVFixed(SQ64FromFloat(0.2))
	if !r.ProtocolRedirectRecommended || !r.ProtocolRedirectToNodes {
		t.Errorf("s=0.2: got %+v", r)
	}
	r = CollateralFromSVFixed(SQ64FromFloat(0.8))
	if !r.ProtocolRedirectRecommended || r.ProtocolRedirectToNodes {
		t.Errorf("s=0.8: got %+v", r)
	}
	r = CollateralFromSVFixed(SQ64FromFloat(0.5))
	if r.ProtocolRedirectRecommended {
		t.Errorf("s=0.5: redirect should not trigger; got %+v", r)
	}
}

func TestProtocolFeeRedirectFixed_MatchesFloat(t *testing.T) {
	for _, s := range []float64{0, 0.1, 0.29, 0.3, 0.31, 0.5, 0.69, 0.7, 0.71, 0.95, 1.0} {
		fl := ProtocolFeeRedirectRecommended(s)
		fx := ProtocolFeeRedirectRecommendedFixed(SQ64FromFloat(s))
		if fl != fx {
			t.Errorf("s=%v: float=%v fixed=%v", s, fl, fx)
		}
	}
}
