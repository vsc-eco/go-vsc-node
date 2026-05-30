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
	// New equilibrium s = 1.0 (V = E). Pick E = 1000, V = 1000, T = 1500,
	// P = 500 (w = P/V = ½), R = 1000. With c = 3:
	//   cE   = 3000
	//   denom = T·V² + P·E·(cE−V) = 1500·1e6 + 500·1000·2000 = 2.5e9
	//   pool  = floor(R · 1e9 / 2.5e9) = 400; node = 1000 − 400 = 600.
	// i.e. 60/40 at equilibrium — equal node/LP yields, since T/V = 1.5.
	in := SplitInputsInt{
		R: biTest("1000"),
		E: biTest("1000"),
		T: biTest("1500"),
		V: biTest("1000"),
		P: biTest("500"),
	}
	out, ok := SplitInt(in)
	if !ok {
		t.Fatal("expected ok")
	}
	wantNode := biTest("600")
	wantPool := biTest("400")
	if out.FinalNodeShare.Cmp(wantNode) != 0 {
		t.Errorf("node = %s want %s", out.FinalNodeShare, wantNode)
	}
	if out.FinalPoolShare.Cmp(wantPool) != 0 {
		t.Errorf("pool = %s want %s", out.FinalPoolShare, wantPool)
	}
	if out.UnderSecured {
		t.Error("unexpected cliff for s=1.0 (equilibrium)")
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

func TestSplitInt_HardCliff_AtCliff(t *testing.T) {
	// New cliff is V >= 3E. s = 3.0 (V = 3E) is the exact cliff.
	in := SplitInputsInt{
		R: biTest("100000"),
		E: biTest("1000"),
		T: biTest("4500"),
		V: biTest("3000"), // s = 3.0, exact cliff (V = 3E)
		P: biTest("1500"),
	}
	out, ok := SplitInt(in)
	if !ok {
		t.Fatal()
	}
	if !out.UnderSecured {
		t.Fatal("expected cliff at V == 3E")
	}
	if out.FinalNodeShare.Cmp(in.R) != 0 || out.FinalPoolShare.Sign() != 0 {
		t.Fatalf("expected (R, 0); got (%s, %s)", out.FinalNodeShare, out.FinalPoolShare)
	}
}

func TestSplitInt_HardCliff_PastCliff(t *testing.T) {
	in := SplitInputsInt{
		R: biTest("100000"),
		E: biTest("1000"),
		T: biTest("10500"),
		V: biTest("3500"), // s = 3.5, past cliff
		P: biTest("1750"),
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

func TestSplitInt_S15_PaysLPs(t *testing.T) {
	// s = 1.5 (V = 1.5E) was a hard cliff in the old model (V ≥ E); under
	// c = 3 it is a normal over-collateralized point that still pays LPs.
	in := SplitInputsInt{
		R: biTest("100000"),
		E: biTest("1000"),
		T: biTest("2250"),
		V: biTest("1500"),
		P: biTest("750"),
	}
	out, ok := SplitInt(in)
	if !ok {
		t.Fatal()
	}
	if out.UnderSecured {
		t.Fatal("s=1.5 must not be a cliff under c=3")
	}
	if out.FinalPoolShare.Sign() <= 0 {
		t.Fatalf("expected positive LP share at s=1.5, got %s", out.FinalPoolShare)
	}
	sum := new(big.Int).Add(out.FinalNodeShare, out.FinalPoolShare)
	if sum.Cmp(in.R) != 0 {
		t.Fatalf("conservation: node+pool=%s want R=%s", sum, in.R)
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
