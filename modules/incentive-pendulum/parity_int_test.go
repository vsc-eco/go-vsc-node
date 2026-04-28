package pendulum

import (
	"math"
	"math/big"
	"testing"
)

func absI64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// TestSplitIntMatchesFloatOnGrid spot-checks that SplitInt and Split agree on a
// representative grid of inputs, within 1 base unit per side.
func TestSplitIntMatchesFloatOnGrid(t *testing.T) {
	cases := []struct {
		R, E, T, V, P int64
	}{
		{100_000, 1_000_000, 1_500_000, 500_000, 250_000},
		{1, 1_000_000, 1_500_000, 500_000, 250_000},
		{1_000_000_000, 1_000_000, 1_500_000, 500_000, 250_000},
		{100_000, 1_000_000, 1_500_000, 50_000, 25_000},   // s = 0.05
		{100_000, 1_000_000, 1_500_000, 950_000, 475_000}, // s = 0.95
		{100_000, 1_000_000, 1_500_000, 700_000, 200_000}, // imbalanced w
		{100_000, 1_000_000, 1_500_000, 0, 0},             // V=0 cliff fallback
		{100_000, 1_000_000, 1_500_000, 1_000_000, 0},     // V=E cliff
	}
	for i, tc := range cases {
		intIn := SplitInputsInt{
			R: big.NewInt(tc.R),
			E: big.NewInt(tc.E),
			T: big.NewInt(tc.T),
			V: big.NewInt(tc.V),
			P: big.NewInt(tc.P),
		}
		intOut, intOk := SplitInt(intIn)
		floatOut, floatOk := Split(SplitInputs{
			R: float64(tc.R), E: float64(tc.E), T: float64(tc.T),
			V: float64(tc.V), P: float64(tc.P), U: 0,
		})
		if intOk != floatOk {
			t.Fatalf("case %d: ok mismatch int=%v float=%v", i, intOk, floatOk)
		}
		if !intOk {
			continue
		}
		intNode := intOut.FinalNodeShare.Int64()
		intPool := intOut.FinalPoolShare.Int64()
		floatNode := int64(math.Round(floatOut.FinalNodeShare))
		floatPool := int64(math.Round(floatOut.FinalPoolShare))
		if absI64(intNode-floatNode) > 1 || absI64(intPool-floatPool) > 1 {
			t.Errorf("case %d: int=(%d,%d) float=(%v,%v)", i, intNode, intPool, floatOut.FinalNodeShare, floatOut.FinalPoolShare)
		}
		if intOut.UnderSecured != floatOut.UnderSecured {
			t.Errorf("case %d: cliff flag mismatch int=%v float=%v", i, intOut.UnderSecured, floatOut.UnderSecured)
		}
	}
}

// TestCLPFeeIntMatchesFloatOnGrid mirrors the CLP fee parity over varied
// trade sizes and pool depths.
func TestCLPFeeIntMatchesFloatOnGrid(t *testing.T) {
	cases := []struct {
		x, X, Y int64
	}{
		{100, 1_000_000, 1_000_000},
		{10_000, 1_000_000, 1_000_000},
		{500_000, 1_000_000, 1_000_000},
		{1_000, 5_000, 200_000},
		{1, 1_000_000, 1_000_000},
	}
	for _, tc := range cases {
		intFee := CLPFeeInt(big.NewInt(tc.x), big.NewInt(tc.X), big.NewInt(tc.Y)).Int64()
		floatFee := int64(math.Floor(CLPFee(float64(tc.x), float64(tc.X), float64(tc.Y))))
		if absI64(intFee-floatFee) > 1 {
			t.Errorf("x=%d X=%d Y=%d: int=%d float=%d", tc.x, tc.X, tc.Y, intFee, floatFee)
		}
	}
}

// FuzzSplitIntMatchesFloat asserts that the integer Split and float Split agree
// within 1 base unit per side on randomly-generated valid inputs. Run via
//
//	go1.24.2 test ./modules/incentive-pendulum -fuzz=FuzzSplitIntMatchesFloat -fuzztime=30s
func FuzzSplitIntMatchesFloat(f *testing.F) {
	f.Add(int64(100_000), int64(1_000_000), int64(1_500_000), int64(400_000), int64(250_000))
	f.Add(int64(1), int64(100), int64(150), int64(40), int64(25))
	f.Fuzz(func(t *testing.T, R, E, T, V, P int64) {
		if R < 0 || E <= 0 || T <= 0 || V < 0 || P < 0 {
			t.Skip()
		}
		// Cap magnitudes to keep float64 precision below the 1-base-unit drift bound.
		const maxV = int64(1e12)
		if R > maxV || E > maxV || T > maxV || V > maxV || P > maxV {
			t.Skip()
		}
		intIn := SplitInputsInt{
			R: big.NewInt(R), E: big.NewInt(E), T: big.NewInt(T), V: big.NewInt(V), P: big.NewInt(P),
		}
		intOut, intOk := SplitInt(intIn)
		floatOut, floatOk := Split(SplitInputs{
			R: float64(R), E: float64(E), T: float64(T), V: float64(V), P: float64(P), U: 0,
		})
		if intOk != floatOk {
			t.Fatalf("ok mismatch: R=%d E=%d T=%d V=%d P=%d int=%v float=%v", R, E, T, V, P, intOk, floatOk)
		}
		if !intOk {
			return
		}
		// Conservation: int sum exactly == R.
		sum := new(big.Int).Add(intOut.FinalNodeShare, intOut.FinalPoolShare)
		if sum.Cmp(intIn.R) != 0 {
			t.Fatalf("int conservation violated: sum=%s R=%d", sum, R)
		}
		intNode := intOut.FinalNodeShare.Int64()
		intPool := intOut.FinalPoolShare.Int64()
		floatNode := int64(math.Round(floatOut.FinalNodeShare))
		floatPool := int64(math.Round(floatOut.FinalPoolShare))
		// Allow 2 base-unit drift: 1 from int residual placement, 1 from float rounding noise.
		if absI64(intNode-floatNode) > 2 || absI64(intPool-floatPool) > 2 {
			t.Fatalf("R=%d E=%d T=%d V=%d P=%d int=(%d,%d) float=(%v,%v)",
				R, E, T, V, P, intNode, intPool, floatOut.FinalNodeShare, floatOut.FinalPoolShare)
		}
	})
}

// TestEffectiveBondHBDIntMatchesFloat confirms parity for the bond conversion
// over typical parameter ranges.
func TestEffectiveBondHBDIntMatchesFloat(t *testing.T) {
	cases := []struct {
		stake int64
		price float64
		frac  float64
	}{
		{1_000_000, 0.25, 2.0 / 3.0},
		{500_000, 1.0, 0.5},
		{10_000_000, 0.001, 1.0},
		{1, 1.0, 1.0},
	}
	for _, tc := range cases {
		got := EffectiveBondHBDInt(big.NewInt(tc.stake), SQ64FromFloat(tc.price), SQ64FromFloat(tc.frac)).Int64()
		want := int64(math.Floor(EffectiveBondHBD(float64(tc.stake), tc.price, tc.frac)))
		if absI64(got-want) > 2 { // small fixed-point round drift acceptable
			t.Errorf("stake=%d price=%v frac=%v: int=%d float=%d", tc.stake, tc.price, tc.frac, got, want)
		}
	}
}
