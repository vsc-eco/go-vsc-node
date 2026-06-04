package ledgerSystem

import (
	"math"
	"math/big"
	"testing"
)

func TestComputeEndingAvg_NormalRange(t *testing.T) {
	// Plain mainnet-shaped numbers; ensure result matches the int64
	// formula on inputs that don't overflow.
	cases := []struct {
		name                       string
		hbdAvg, hbdSavings, a, b int64
		want                       int64
	}{
		{"all-zero", 0, 0, 0, 1, 0},
		{"flat balance", 0, 1000, 100, 100, 1000},
		{"with prior avg", 5000, 1000, 100, 100, 1050}, // (5000 + 1000*100)/100 = 105000/100 = 1050
		{"long interval", 0, 1_000_000, 1_000_000, 2_000_000, 500_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := computeEndingAvg(c.hbdAvg, c.hbdSavings, c.a, c.b)
			if !ok {
				t.Fatalf("ok=false; want true for in-range inputs")
			}
			if got != c.want {
				t.Fatalf("got=%d want=%d", got, c.want)
			}
		})
	}
}

func TestComputeEndingAvg_OverflowFailsClosed(t *testing.T) {
	// HBD_SAVINGS * A on int64 wraps. Helper must reject without panicking.
	// Pick values so the product exceeds 2^63-1 (~9.22e18).
	hbdSavings := int64(1) << 40 // ~1.1e12
	a := int64(1) << 24          // ~1.7e7 — product ~1.9e19 > int64
	b := int64(1)
	if _, ok := computeEndingAvg(0, hbdSavings, a, b); ok {
		t.Fatalf("expected ok=false on overflow")
	}
}

func TestComputeEndingAvg_BoundaryFitsInt64(t *testing.T) {
	// (math.MaxInt64 / 2) divided by 1 fits exactly; ensure helper accepts it.
	got, ok := computeEndingAvg(0, math.MaxInt64/2, 1, 1)
	if !ok {
		t.Fatalf("ok=false on in-range boundary")
	}
	if got != math.MaxInt64/2 {
		t.Fatalf("got=%d want=%d", got, int64(math.MaxInt64/2))
	}
}

func TestComputeDistributeAmount_Normal(t *testing.T) {
	// HBD_AVG=1000, amount=100, totalAvg=10_000 → 1000*100/10_000 = 10
	got, ok := computeDistributeAmount(1000, 100, big.NewInt(10_000))
	if !ok || got != 10 {
		t.Fatalf("got=%d ok=%v want=10 ok=true", got, ok)
	}
}

func TestComputeDistributeAmount_ZeroTotalAvgFailsClosed(t *testing.T) {
	if _, ok := computeDistributeAmount(1000, 100, big.NewInt(0)); ok {
		t.Fatalf("expected ok=false on totalAvg=0")
	}
	// GV-L2: a nil denominator must also fail closed, not panic.
	if _, ok := computeDistributeAmount(1000, 100, nil); ok {
		t.Fatalf("expected ok=false on nil totalAvg")
	}
	// GV-L2: a negative denominator (what the pre-fix int64 accumulator
	// produced on overflow) must fail closed, never return a negative share.
	if _, ok := computeDistributeAmount(1000, 100, big.NewInt(-1)); ok {
		t.Fatalf("expected ok=false on negative totalAvg")
	}
}

func TestComputeDistributeAmount_LargeOperands(t *testing.T) {
	// Large HBD_AVG * amount would overflow int64, but final ratio is small
	// because totalAvg matches the same magnitude. Helper must NOT
	// fail-closed in that case.
	hbdAvg := int64(1) << 50
	amount := int64(1) << 50
	totalAvg := new(big.Int).Lsh(big.NewInt(1), 50)
	got, ok := computeDistributeAmount(hbdAvg, amount, totalAvg)
	if !ok {
		t.Fatalf("ok=false on large-but-quotient-in-range inputs")
	}
	if got != hbdAvg {
		t.Fatalf("got=%d want=%d", got, hbdAvg)
	}
}
