package pendulum

import "testing"

// TestParams_CliffDerivation pins c = 2·s_eq² + s_eq at the live target.
func TestParams_CliffDerivation(t *testing.T) {
	if CliffSBps != 30_000 {
		t.Fatalf("CliffSBps = %d, want 30000 (c=3.0 at s_eq=1.0)", CliffSBps)
	}
}

// TestParams_DerivedBandEdges locks the band s-edges to an independently
// computed oracle table (floor of the integer sqrt inversion at c=3.0). If the
// derivation drifts, this catches it.
func TestParams_DerivedBandEdges(t *testing.T) {
	cases := []struct {
		name string
		got  int64
		want int64
	}{
		{"extremeLo", extremeLoEdgeBps, 3_631},
		{"warnLo", warnLoEdgeBps, 4_601},
		{"safeLo", safeLoEdgeBps, 5_600},
		{"idealLo", idealLoEdgeBps, 8_829},
		{"idealHi", idealHiEdgeBps, 11_232},
		{"safeHi", safeHiEdgeBps, 15_427},
		{"warnHi", warnHiEdgeBps, 17_061},
		{"extremeHi", extremeHiEdgeBps, 21_097},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s edge = %d, want %d", c.name, c.got, c.want)
		}
	}
	if RedirectLoBps != safeLoEdgeBps || RedirectHiBps != safeHiEdgeBps {
		t.Errorf("redirect edges must equal safe-band edges: lo %d/%d hi %d/%d",
			RedirectLoBps, safeLoEdgeBps, RedirectHiBps, safeHiEdgeBps)
	}
}

// TestParams_YieldRatioRoundTrip confirms each derived s-edge is a level set of
// the yield curve: ρ(edge) ≈ its source threshold (within integer-floor slack,
// ~1%). The exact edges are pinned by TestParams_DerivedBandEdges; this asserts
// the inversion is the genuine inverse of YieldRatioBps.
func TestParams_YieldRatioRoundTrip(t *testing.T) {
	cases := []struct {
		edge, ratio int64
	}{
		{extremeLoEdgeBps, extremeRatioLoBps},
		{warnLoEdgeBps, warnRatioLoBps},
		{safeLoEdgeBps, safeRatioLoBps},
		{idealLoEdgeBps, idealRatioLoBps},
		{idealHiEdgeBps, idealRatioHiBps},
		{safeHiEdgeBps, safeRatioHiBps},
		{warnHiEdgeBps, warnRatioHiBps},
		{extremeHiEdgeBps, extremeRatioHiBps},
	}
	for _, c := range cases {
		got := YieldRatioBps(c.edge)
		tol := c.ratio/100 + 5 // ~1% + a few bps of floor slack
		if diff := got - c.ratio; diff > tol || diff < -tol {
			t.Errorf("ratio(%d) = %d, want ≈ %d (tol %d)", c.edge, got, c.ratio, tol)
		}
	}
	// Equilibrium: equal yields ⇒ ratio = 1.0.
	if r := YieldRatioBps(TargetSBps); r < 9_995 || r > 10_005 {
		t.Errorf("ratio at s_eq = %d, want ≈ 10000 (equal yields)", r)
	}
}

// TestParams_FaithfulnessAtOldTarget proves the parameterization is
// behavior-preserving: re-running the derivation at s_eq = 0.5 reproduces the
// historical c = 1.0 cliff and the 0.30/0.70 safe band the old fixed literals
// encoded.
func TestParams_FaithfulnessAtOldTarget(t *testing.T) {
	oldCliff := deriveCliffSBps(5_000)
	if oldCliff != 10_000 {
		t.Fatalf("c at s_eq=0.5 = %d, want 10000 (1.0)", oldCliff)
	}
	if lo := sEdgeForRatio(safeRatioLoBps, oldCliff); lo < 2_998 || lo > 3_002 {
		t.Errorf("safe-lo at old target = %d, want ≈ 3000 (0.30)", lo)
	}
	if hi := sEdgeForRatio(safeRatioHiBps, oldCliff); hi < 6_998 || hi > 7_002 {
		t.Errorf("safe-hi at old target = %d, want ≈ 7000 (0.70)", hi)
	}
}
