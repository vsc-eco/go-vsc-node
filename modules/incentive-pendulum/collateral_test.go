package pendulum

import "testing"

func TestCollateralFromSVBands(t *testing.T) {
	cases := []struct {
		s        float64
		under    bool
		ideal    bool
		warn     bool
		extremeL bool
	}{
		// s=1: hard cliff; also satisfies s > 0.75 → warning band in parallel.
		{1.0, true, false, true, false},
		{0.5, false, true, false, false},
		{0.45, false, true, false, false},
		{0.55, false, true, false, false},
		{0.44, false, false, false, false},
		{0.56, false, false, false, false},
		{0.76, false, false, true, false},
		{0.24, false, false, true, false},
		{0.19, false, false, true, true},
	}
	for _, tc := range cases {
		r := CollateralFromSV(tc.s)
		if r.UnderSecured != tc.under || r.IdealZone != tc.ideal || r.WarningZone != tc.warn || r.ExtremeLow != tc.extremeL {
			t.Fatalf("s=%v got %+v want under=%v ideal=%v warn=%v extreme=%v", tc.s, r, tc.under, tc.ideal, tc.warn, tc.extremeL)
		}
	}
}

func TestEffectiveBondHBD(t *testing.T) {
	if v := EffectiveBondHBD(1000, 0.3, 2.0/3.0); v != 200 {
		t.Fatal(v)
	}
	if v := EffectiveBondHBD(0, 1, 1); v != 0 {
		t.Fatal(v)
	}
}
