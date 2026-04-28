package pendulum

import (
	"math/big"
	"testing"
)

func biTest(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad bigint: " + s)
	}
	return v
}

func TestSplitInt_BasicMatchesPDF(t *testing.T) {
	// Mirrors TestPendulumBoltEvaluate fixture in property_test.go but in base units.
	// E = 10000 * 0.25 * 2/3 ≈ 1666.67 → use exact integer 5000/3 ≈ 1666 (we'll feed
	// a clean integer geometry instead).
	// Let s = 0.5: V = E. So pick E = 1_000_000, V = 500_000, P = 250_000, T = 1_500_000.
	// u = T/E = 1.5, w = P/V = 0.5.
	// denom = 1.5·0.5 + 0.5·0.5 = 1.0
	// FinalNodeShare = R · 0.75 / 1.0; FinalPoolShare = R · 0.25 / 1.0.
	in := SplitInputsInt{
		R: biTest("1000000"), // 1e6 base units
		E: biTest("1000000"),
		T: biTest("1500000"),
		V: biTest("500000"),
		P: biTest("250000"),
	}
	out, ok := SplitInt(in)
	if !ok {
		t.Fatal("expected ok")
	}
	wantNode := biTest("750000")
	wantPool := biTest("250000")
	if out.FinalNodeShare.Cmp(wantNode) != 0 {
		t.Errorf("node = %s want %s", out.FinalNodeShare, wantNode)
	}
	if out.FinalPoolShare.Cmp(wantPool) != 0 {
		t.Errorf("pool = %s want %s", out.FinalPoolShare, wantPool)
	}
	if out.UnderSecured {
		t.Error("unexpected cliff for s=0.5")
	}
}

func TestSplitInt_ConservesRExactly(t *testing.T) {
	// Conservation: node + pool == R for every valid input.
	cases := []SplitInputsInt{
		{R: biTest("12345"), E: biTest("100"), T: biTest("150"), V: biTest("40"), P: biTest("25")},
		{R: biTest("1"), E: biTest("100"), T: biTest("150"), V: biTest("40"), P: biTest("25")},
		{R: biTest("0"), E: biTest("100"), T: biTest("150"), V: biTest("40"), P: biTest("25")},
		{R: biTest("999999999999"), E: biTest("100"), T: biTest("150"), V: biTest("40"), P: biTest("25")},
		// Pool numerator share is a non-clean fraction — forces residual.
		{R: biTest("100"), E: biTest("7"), T: biTest("11"), V: biTest("3"), P: biTest("2")},
	}
	for i, in := range cases {
		out, ok := SplitInt(in)
		if !ok {
			t.Fatalf("case %d: expected ok", i)
		}
		sum := new(big.Int).Add(out.FinalNodeShare, out.FinalPoolShare)
		if sum.Cmp(in.R) != 0 {
			t.Fatalf("case %d: sum=%s R=%s node=%s pool=%s", i, sum, in.R, out.FinalNodeShare, out.FinalPoolShare)
		}
	}
}

func TestSplitInt_HardCliff_VEqualsE(t *testing.T) {
	in := SplitInputsInt{
		R: biTest("100000"),
		E: biTest("1000"),
		T: biTest("1500"),
		V: biTest("1000"), // s = 1.0, exact cliff
		P: biTest("500"),
	}
	out, ok := SplitInt(in)
	if !ok {
		t.Fatal()
	}
	if !out.UnderSecured {
		t.Fatal("expected cliff at V == E")
	}
	if out.FinalNodeShare.Cmp(in.R) != 0 || out.FinalPoolShare.Sign() != 0 {
		t.Fatalf("expected (R, 0); got (%s, %s)", out.FinalNodeShare, out.FinalPoolShare)
	}
}

func TestSplitInt_HardCliff_VGreaterThanE(t *testing.T) {
	in := SplitInputsInt{
		R: biTest("100000"),
		E: biTest("1000"),
		T: biTest("3000"),
		V: biTest("1500"), // s = 1.5
		P: biTest("750"),
	}
	out, ok := SplitInt(in)
	if !ok {
		t.Fatal()
	}
	if !out.UnderSecured {
		t.Fatal()
	}
	if out.FinalNodeShare.Cmp(in.R) != 0 || out.FinalPoolShare.Sign() != 0 {
		t.Fatalf("got (%s, %s)", out.FinalNodeShare, out.FinalPoolShare)
	}
}

func TestSplitInt_VZero_FallsBackToNodesAll(t *testing.T) {
	// Mirrors float fallback at V == 0 (denom == 0 in the original formula).
	in := SplitInputsInt{
		R: biTest("100000"),
		E: biTest("1000"),
		T: biTest("1500"),
		V: biTest("0"),
		P: biTest("0"),
	}
	out, ok := SplitInt(in)
	if !ok {
		t.Fatal()
	}
	if out.FinalNodeShare.Cmp(in.R) != 0 || out.FinalPoolShare.Sign() != 0 {
		t.Fatalf("got (%s, %s)", out.FinalNodeShare, out.FinalPoolShare)
	}
}

func TestSplitInt_VZero_PPositive_FallsBackToNodesAll(t *testing.T) {
	// V=0 but P>0 is geometrically inconsistent (HBD without vault). Match float behaviour:
	// fallback hands all to nodes.
	in := SplitInputsInt{
		R: biTest("100000"),
		E: biTest("1000"),
		T: biTest("1500"),
		V: biTest("0"),
		P: biTest("500"),
	}
	out, ok := SplitInt(in)
	if !ok {
		t.Fatal()
	}
	if out.FinalNodeShare.Cmp(in.R) != 0 || out.FinalPoolShare.Sign() != 0 {
		t.Fatalf("got (%s, %s)", out.FinalNodeShare, out.FinalPoolShare)
	}
}

func TestSplitInt_PZero_VPositive(t *testing.T) {
	// w = P/V = 0; denom = u*s + 0 = TV/E². Pool gets nothing; node gets R.
	in := SplitInputsInt{
		R: biTest("100000"),
		E: biTest("1000"),
		T: biTest("1500"),
		V: biTest("400"),
		P: biTest("0"),
	}
	out, ok := SplitInt(in)
	if !ok {
		t.Fatal()
	}
	if out.FinalNodeShare.Cmp(in.R) != 0 || out.FinalPoolShare.Sign() != 0 {
		t.Fatalf("got (%s, %s)", out.FinalNodeShare, out.FinalPoolShare)
	}
}

func TestSplitInt_InvalidInputs(t *testing.T) {
	// E <= 0
	if _, ok := SplitInt(SplitInputsInt{R: biTest("1"), E: biTest("0"), T: biTest("1"), V: biTest("0"), P: biTest("0")}); ok {
		t.Error("E=0 should be !ok")
	}
	// T <= 0
	if _, ok := SplitInt(SplitInputsInt{R: biTest("1"), E: biTest("1"), T: biTest("0"), V: biTest("0"), P: biTest("0")}); ok {
		t.Error("T=0 should be !ok")
	}
	// nil R
	if _, ok := SplitInt(SplitInputsInt{R: nil, E: biTest("1"), T: biTest("1"), V: biTest("0"), P: biTest("0")}); ok {
		t.Error("nil R should be !ok")
	}
}

func TestSplitInt_NegativeInputsRejected(t *testing.T) {
	if _, ok := SplitInt(SplitInputsInt{R: biTest("-1"), E: biTest("1"), T: biTest("1"), V: biTest("1"), P: biTest("1")}); ok {
		t.Error("negative R should be !ok")
	}
	if _, ok := SplitInt(SplitInputsInt{R: biTest("1"), E: biTest("1"), T: biTest("1"), V: biTest("-1"), P: biTest("1")}); ok {
		t.Error("negative V should be !ok")
	}
}

func TestSplitInt_DoesNotMutateInputs(t *testing.T) {
	in := SplitInputsInt{
		R: biTest("100000"),
		E: biTest("1000"),
		T: biTest("1500"),
		V: biTest("400"),
		P: biTest("250"),
	}
	snap := []*big.Int{
		new(big.Int).Set(in.R),
		new(big.Int).Set(in.E),
		new(big.Int).Set(in.T),
		new(big.Int).Set(in.V),
		new(big.Int).Set(in.P),
	}
	_, _ = SplitInt(in)
	if in.R.Cmp(snap[0]) != 0 || in.E.Cmp(snap[1]) != 0 || in.T.Cmp(snap[2]) != 0 || in.V.Cmp(snap[3]) != 0 || in.P.Cmp(snap[4]) != 0 {
		t.Fatal("SplitInt mutated inputs")
	}
}
