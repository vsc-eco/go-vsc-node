package pendulum

import (
	"math"
	"testing"
)

func approxEq(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

// Table 2 (PDF), u = 1.5, w = 2/3, R = 1, E = 1 for normalized yields.
func TestTable2Behaviour(t *testing.T) {
	const u = 1.5
	const w = 2.0 / 3.0
	const R, E, T = 1.0, 1.0, 1.0

	cases := []struct {
		s           float64
		denom       float64
		pctNode     float64
		pctPool     float64
		nodeYieldRE float64
		poolYieldRE float64
		ratio       float64
	}{
		{0.05, 0.7083, 10.6, 89.4, 0.0706, 13.4118, 0.01},
		{0.10, 0.7500, 20.0, 80.0, 0.1333, 6.0000, 0.02},
		{0.20, 0.8333, 36.0, 64.0, 0.2400, 2.4000, 0.10},
		{0.30, 0.9167, 49.1, 50.9, 0.3273, 1.2727, 0.26},
		{0.35, 0.9583, 54.8, 45.2, 0.3652, 0.9689, 0.38},
		{0.40, 1.0000, 60.0, 40.0, 0.4000, 0.7500, 0.53},
		{0.45, 1.0417, 64.8, 35.2, 0.4320, 0.5867, 0.74},
		{0.50, 1.0833, 69.2, 30.8, 0.4615, 0.4615, 1.00},
		{0.55, 1.1250, 73.3, 26.7, 0.4889, 0.3636, 1.34},
		{0.60, 1.1667, 77.1, 22.9, 0.5143, 0.2857, 1.80},
		{0.65, 1.2083, 80.7, 19.3, 0.5379, 0.2228, 2.41},
		{0.70, 1.2500, 84.0, 16.0, 0.5600, 0.1714, 3.27},
		{0.80, 1.3333, 90.0, 10.0, 0.6000, 0.0938, 6.40},
		{0.90, 1.4167, 95.3, 4.7, 0.6353, 0.0392, 16.20},
		{0.95, 1.4583, 97.7, 2.3, 0.6514, 0.0180, 36.10},
	}

	const eps = 0.05 // table is rounded; allow small drift vs full-precision formulas

	for _, tc := range cases {
		V := tc.s * E
		P := w * V
		out, ok := Split(SplitInputs{R: R, E: E, T: T, V: V, P: P, U: u})
		if !ok {
			t.Fatalf("s=%v Split not ok", tc.s)
		}
		if out.UnderSecured {
			t.Fatalf("s=%v unexpectedly under-secured", tc.s)
		}

		denom := u*tc.s + w*(1-tc.s)
		if !approxEq(denom, tc.denom, 0.002) {
			t.Errorf("s=%.2f denom want ~%.4f got %.4f", tc.s, tc.denom, denom)
		}

		pctN := 100 * out.FinalNodeShare / R
		pctP := 100 * out.FinalPoolShare / R
		if !approxEq(pctN, tc.pctNode, eps) {
			t.Errorf("s=%.2f %%node want ~%.1f got %.2f", tc.s, tc.pctNode, pctN)
		}
		if !approxEq(pctP, tc.pctPool, eps) {
			t.Errorf("s=%.2f %%pool want ~%.1f got %.2f", tc.s, tc.pctPool, pctP)
		}

		nodeRE := out.NodeYield / (R / E)
		poolRE := out.PoolYield / (R / E)
		if !approxEq(nodeRE, tc.nodeYieldRE, eps) {
			t.Errorf("s=%.2f nodeYield×(R/E) want ~%.4f got %.4f", tc.s, tc.nodeYieldRE, nodeRE)
		}
		if !approxEq(poolRE, tc.poolYieldRE, eps) {
			t.Errorf("s=%.2f poolYield×(R/E) want ~%.4f got %.4f", tc.s, tc.poolYieldRE, poolRE)
		}

		if out.PoolYield > 0 {
			rat := out.NodeYield / out.PoolYield
			if !approxEq(rat, tc.ratio, eps) {
				t.Errorf("s=%.2f node/pool ratio want ~%.2f got %.2f", tc.s, tc.ratio, rat)
			}
		}
	}
}

func TestSplitHardCliff(t *testing.T) {
	out, ok := Split(SplitInputs{R: 100, E: 10, T: 30, V: 15, P: 5, U: 1.5})
	if !ok {
		t.Fatal("expected ok")
	}
	if !out.UnderSecured {
		t.Fatal("expected under-secured s>=1")
	}
	if out.FinalNodeShare != 100 || out.FinalPoolShare != 0 {
		t.Fatal("expected all R to nodes")
	}
	if !approxEq(out.NodeYield, 100.0/30.0, 1e-9) {
		t.Fatal("nodeYield")
	}
}

func TestYieldRatioIdentity(t *testing.T) {
	for _, s := range []float64{0.1, 0.25, 0.5, 0.75, 0.9} {
		yr := YieldRatio(s)
		want := (2 * s * s) / (1 - s)
		if !approxEq(yr, want, 1e-12) {
			t.Errorf("s=%v", s)
		}
	}
}

func TestCLPFeeBalancedPool(t *testing.T) {
	// x=0.01% of X => r = 0.0001; use X=Y=1e6, x=100
	X, Y := 1_000_000.0, 1_000_000.0
	x := 100.0
	fee := CLPFee(x, X, Y)
	// x^2 Y / (x+X)^2 = 1e4 * 1e6 / (1.0001e6)^2
	want := (x * x * Y) / ((x + X) * (x + X))
	if !approxEq(fee, want, 1e-6) {
		t.Fatalf("CLP fee %v want %v", fee, want)
	}
}

func TestPendulumFeeFractionTable1Micro(t *testing.T) {
	r := 0.0001 // 0.01% of pool
	f := PendulumFeeFraction(r)
	if f < 0.10 || f > 0.12 {
		t.Fatalf("expected ~11%% for micro trade, got %.4f", f)
	}
}

func TestStabilizerAtEquilibrium(t *testing.T) {
	p := DefaultStabilizerParams()
	m := StabilizerMultiplier(0.5, 0.05, p)
	if m != 1.0 {
		t.Fatalf("at s=0.5 want 1.0 got %v", m)
	}
}
