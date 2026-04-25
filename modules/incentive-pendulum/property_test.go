package pendulum

import (
	"math"
	"testing"
)

func TestSplitConservesR(t *testing.T) {
	const eps = 1e-9
	rs := []float64{0, 1, 1e6, 3.14159}
	for _, R := range rs {
		for _, E := range []float64{100.0, 500.0} {
			for _, T := range []float64{150.0, 200.0} {
				for _, f := range []float64{0.05, 0.2, 0.5, 0.8, 0.95} {
					V := f * 0.99 * E // s < 1
					P := (2.0 / 3.0) * V
					out, ok := Split(SplitInputs{R: R, E: E, T: T, V: V, P: P, U: 1.5})
					if !ok {
						t.Fatalf("R=%v E=%v", R, E)
					}
					sum := out.FinalNodeShare + out.FinalPoolShare
					if math.Abs(sum-R) > eps*math.Max(1, math.Abs(R)) {
						t.Fatalf("R=%v sum=%v node=%v pool=%v", R, sum, out.FinalNodeShare, out.FinalPoolShare)
					}
				}
			}
		}
	}
}

func TestSplitConservesRHardCliff(t *testing.T) {
	R := 12345.0
	out, ok := Split(SplitInputs{R: R, E: 10, T: 30, V: 15, P: 5, U: 1.5})
	if !ok || !out.UnderSecured {
		t.Fatal()
	}
	if out.FinalNodeShare+out.FinalPoolShare != R {
		t.Fatal()
	}
}

func TestYieldRatioMatchesSplit(t *testing.T) {
	const R, E, T = 1.0, 1.0, 1.5
	const u, w = 1.5, 2.0 / 3.0
	for _, s := range []float64{0.2, 0.35, 0.5, 0.65, 0.8} {
		V := s * E
		P := w * V
		out, ok := Split(SplitInputs{R: R, E: E, T: T, V: V, P: P, U: u})
		if !ok || out.PoolYield <= 0 {
			t.Fatal(s)
		}
		yr := out.NodeYield / out.PoolYield
		want := YieldRatio(s)
		if math.Abs(yr-want) > 1e-9 {
			t.Fatalf("s=%v yr=%v want=%v", s, yr, want)
		}
	}
}

func TestQuoteSwapFeesSurplusNonNegativeWhenMultiplierGte1(t *testing.T) {
	stab := DefaultStabilizerParams()
	x, X, Y := 5000.0, 1_000_000.0, 1_000_000.0
	for _, s := range []float64{0.3, 0.5, 0.8} {
		for _, ex := range []bool{true, false} {
			q := QuoteSwapFees(x, SwapLegDepths{X: X, Y: Y}, s, ex, stab)
			if q.Multiplier < 1 {
				t.Fatal()
			}
			if q.ChargedTotal+1e-12 < q.BaseSubtotal {
				t.Fatal()
			}
			if math.Abs(q.ChargedTotal-q.BaseSubtotal*q.Multiplier) > 1e-6 {
				t.Fatal()
			}
			if q.AccrueToPendulumR != q.BaseCLP {
				t.Fatal()
			}
		}
	}
}

func TestSumPendulumVault(t *testing.T) {
	v, p := SumPendulumVault([]PoolPendulumLiquidity{
		{PoolID: "a", Owner: "hive:vsc.dao", PHbd: 100},
		{PoolID: "b", Owner: "hive:vsc.dao", PHbd: 50},
	})
	if p != 150 || v != 300 {
		t.Fatalf("v=%v p=%v", v, p)
	}
}

func TestPendulumBoltEvaluate(t *testing.T) {
	b := NewPendulumBolt()
	// E = 10000 * 0.25 * 2/3 ≈ 1666.67; V = 2*300 = 600 → s ≈ 0.36 (safe growth band).
	ev, ok := b.Evaluate(NetworkSnapshot{
		TotalHiveStake: 10_000,
		HivePriceHBD:   0.25,
		TotalBondT:     5000,
		Pools:          []PoolPendulumLiquidity{{PoolID: "p1", Owner: "hive:vsc.dao", PHbd: 300}},
	}, 1e6)
	if !ok {
		t.Fatal("expected ok")
	}
	// E = 10000 * 0.25 * 2/3
	wantE := 10_000.0 * 0.25 * (2.0 / 3.0)
	if math.Abs(ev.E-wantE) > 1e-6 {
		t.Fatalf("E %v want %v", ev.E, wantE)
	}
	if ev.V != 600 || ev.P != 300 {
		t.Fatal(ev.V, ev.P)
	}
	sum := ev.Split.FinalNodeShare + ev.Split.FinalPoolShare
	if math.Abs(sum-1e6) > 1e-3 {
		t.Fatalf("split sum got %v want 1e6", sum)
	}
	if !ev.Collateral.SafeGrowth {
		t.Fatal("expected safe growth for typical s")
	}
}

func TestPendulumBoltEvaluateDAOOnlyFilter(t *testing.T) {
	b := NewPendulumBolt()
	ev, ok := b.Evaluate(NetworkSnapshot{
		TotalHiveStake: 10_000,
		HivePriceHBD:   0.25,
		TotalBondT:     5000,
		Pools: []PoolPendulumLiquidity{
			{PoolID: "dao-pool", Owner: "hive:vsc.dao", PHbd: 300},
			{PoolID: "user-pool", Owner: "hive:alice", PHbd: 700},
		},
	}, 1e6)
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.P != 300 || ev.V != 600 {
		t.Fatalf("DAO filter failed, got V=%v P=%v", ev.V, ev.P)
	}
}

func TestPendulumBoltEvaluateCanDisableDAOFilter(t *testing.T) {
	b := NewPendulumBolt()
	b.EnforceDAOOwnedPools = false
	ev, ok := b.Evaluate(NetworkSnapshot{
		TotalHiveStake: 10_000,
		HivePriceHBD:   0.25,
		TotalBondT:     5000,
		Pools: []PoolPendulumLiquidity{
			{PoolID: "dao-pool", Owner: "hive:vsc.dao", PHbd: 300},
			{PoolID: "user-pool", Owner: "hive:alice", PHbd: 700},
		},
	}, 1e6)
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.P != 1000 || ev.V != 2000 {
		t.Fatalf("expected all pools included, got V=%v P=%v", ev.V, ev.P)
	}
}

func TestEvaluateMissingT(t *testing.T) {
	b := NewPendulumBolt()
	ev, ok := b.Evaluate(NetworkSnapshot{
		TotalHiveStake: 10_000,
		HivePriceHBD:   0.25,
		TotalBondT:     0,
		Pools:          nil,
	}, 0)
	if ok {
		t.Fatal("expected !ok when T missing")
	}
	if ev.E <= 0 {
		t.Fatal("E should still be computed")
	}
}

func TestPoolOwnedByAcceptsHivePrefixVariants(t *testing.T) {
	if !PoolOwnedBy("vsc.dao", "hive:vsc.dao") {
		t.Fatal("expected owner match")
	}
	if !PoolOwnedBy("HIVE:VSC.DAO", "hive:vsc.dao") {
		t.Fatal("expected case-insensitive owner match")
	}
	if PoolOwnedBy("hive:alice", "hive:vsc.dao") {
		t.Fatal("unexpected owner match")
	}
}

func FuzzSplitConservesR(f *testing.F) {
	f.Add(100.0, 150.0, 40.0, 25.0, 1.5, 1000.0)
	f.Fuzz(func(t *testing.T, E, T, V, P, U, R float64) {
		if E <= 0 || T <= 0 || R < 0 || U < 0 {
			t.Skip()
		}
		if V < 0 || P < 0 {
			t.Skip()
		}
		out, ok := Split(SplitInputs{R: R, E: E, T: T, V: V, P: P, U: U})
		if !ok {
			t.Skip()
		}
		sum := out.FinalNodeShare + out.FinalPoolShare
		if math.Abs(sum-R) > 1e-6*math.Max(1, math.Abs(R)) {
			t.Fatalf("sum=%v R=%v", sum, R)
		}
		if out.FinalNodeShare < -1e-9 || out.FinalPoolShare < -1e-9 {
			t.Fatal("negative share")
		}
	})
}
