package intmath

import (
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
