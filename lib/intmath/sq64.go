package intmath

import "math/big"

// SQ64 is a signed 8-decimal fixed-point number stored as int64.
// 1.0 == SQ64Scale; 0.5 == SQ64Scale/2; 0.0008 == 80_000.
//
// Range: representable values up to ~9.22e10 in real terms, which covers every
// pendulum ratio (s, w, multiplier) and small fractional rates by huge margin.
// Multiplications use big.Int internally to avoid intermediate overflow.
type SQ64 int64

// SQ64Scale is 10^8.
const SQ64Scale int64 = 100_000_000

var sq64ScaleBig = big.NewInt(SQ64Scale)

// SQ64FromFloat converts a float to SQ64. Use only at boundaries where a
// float-typed governance parameter is loaded; never inside consensus paths.
func SQ64FromFloat(f float64) SQ64 {
	if f >= 0 {
		return SQ64(int64(f*float64(SQ64Scale) + 0.5))
	}
	return SQ64(int64(f*float64(SQ64Scale) - 0.5))
}

// ToFloat converts SQ64 back to float64 for display / float-comparison tests.
func (q SQ64) ToFloat() float64 {
	return float64(q) / float64(SQ64Scale)
}

// SQ64Mul returns a*b in SQ64, using big.Int internally to avoid overflow.
func SQ64Mul(a, b SQ64) SQ64 {
	prod := new(big.Int).Mul(big.NewInt(int64(a)), big.NewInt(int64(b)))
	prod.Quo(prod, sq64ScaleBig)
	return SQ64(prod.Int64())
}

// SQ64Div returns a/b in SQ64, ok=false on b == 0.
func SQ64Div(a, b SQ64) (SQ64, bool) {
	if b == 0 {
		return 0, false
	}
	num := new(big.Int).Mul(big.NewInt(int64(a)), sq64ScaleBig)
	num.Quo(num, big.NewInt(int64(b)))
	return SQ64(num.Int64()), true
}

// SQ64Abs returns |a|.
func SQ64Abs(a SQ64) SQ64 {
	if a < 0 {
		return -a
	}
	return a
}
