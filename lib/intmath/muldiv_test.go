package intmath

import (
	"math"
	"math/big"
	"testing"
)

func bi(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad bigint: " + s)
	}
	return v
}

func TestMulDivFloor_Basic(t *testing.T) {
	cases := []struct {
		a, b, c, want string
	}{
		{"6", "7", "3", "14"},     // 42/3 = 14
		{"7", "7", "3", "16"},     // 49/3 = 16.33 → 16 (floor)
		{"0", "5", "1", "0"},      // 0
		{"1", "0", "1", "0"},      // 0
		{"100", "200", "1", "20000"},
		{"100", "200", "7", "2857"}, // 20000/7 = 2857.14 → 2857
	}
	for _, tc := range cases {
		got := MulDivFloor(bi(tc.a), bi(tc.b), bi(tc.c))
		if got.String() != tc.want {
			t.Errorf("MulDivFloor(%s,%s,%s) = %s, want %s", tc.a, tc.b, tc.c, got.String(), tc.want)
		}
	}
}

func TestMulDivFloor_LargeNoOverflow(t *testing.T) {
	// 10^25 * 10^25 / 10^20 = 10^30 — exceeds int64 by far.
	a := bi("10000000000000000000000000")
	b := bi("10000000000000000000000000")
	c := bi("100000000000000000000")
	want := bi("1000000000000000000000000000000")
	got := MulDivFloor(a, b, c)
	if got.Cmp(want) != 0 {
		t.Errorf("got %s want %s", got.String(), want.String())
	}
}

func TestMulDivFloor_DoesNotMutateInputs(t *testing.T) {
	a := bi("123")
	b := bi("456")
	c := bi("7")
	aSnap := new(big.Int).Set(a)
	bSnap := new(big.Int).Set(b)
	cSnap := new(big.Int).Set(c)
	_ = MulDivFloor(a, b, c)
	if a.Cmp(aSnap) != 0 || b.Cmp(bSnap) != 0 || c.Cmp(cSnap) != 0 {
		t.Fatalf("MulDivFloor mutated inputs: a=%s b=%s c=%s", a, b, c)
	}
}

func TestMulDivFloor_ZeroDivisorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on zero divisor")
		}
	}()
	MulDivFloor(bi("1"), bi("1"), bi("0"))
}

func TestMulDivFloorI64_Basic(t *testing.T) {
	cases := []struct {
		a, b, c int64
		want    int64
	}{
		{6, 7, 3, 14},     // 42/3
		{7, 7, 3, 16},     // 49/3 → 16 (floor)
		{0, 5, 1, 0},      // zero a
		{1, 0, 1, 0},      // zero b
		{100, 200, 7, 2857},
	}
	for _, tc := range cases {
		got, ok := MulDivFloorI64(tc.a, tc.b, tc.c)
		if !ok {
			t.Errorf("MulDivFloorI64(%d,%d,%d): unexpected !ok", tc.a, tc.b, tc.c)
			continue
		}
		if got != tc.want {
			t.Errorf("MulDivFloorI64(%d,%d,%d) = %d, want %d", tc.a, tc.b, tc.c, got, tc.want)
		}
	}
}

func TestMulDivFloorI64_OverflowReturnsNotOk(t *testing.T) {
	// MaxInt64 * MaxInt64 / 1 ≫ MaxInt64. Must return ok=false so callers
	// (StabilizerMultiplierBps in particular) abort the swap rather than
	// silently treating overflow as zero.
	got, ok := MulDivFloorI64(math.MaxInt64, math.MaxInt64, 1)
	if ok {
		t.Fatalf("expected !ok on overflow, got=%d ok=%v", got, ok)
	}
	if got != 0 {
		t.Errorf("expected 0 on overflow, got=%d", got)
	}
}

func TestMulDivFloorI64_LargeFitsAfterDivision(t *testing.T) {
	// (MaxInt64) * 10 / 10 == MaxInt64 — intermediate product overflows
	// int64 but the floor fits, so big.Int promotion saves us.
	got, ok := MulDivFloorI64(math.MaxInt64, 10, 10)
	if !ok {
		t.Fatalf("expected ok, got !ok")
	}
	if got != math.MaxInt64 {
		t.Errorf("got %d want %d", got, math.MaxInt64)
	}
}

func TestMulDivFloorI64_ZeroDivisorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on zero divisor")
		}
	}()
	MulDivFloorI64(1, 1, 0)
}
