package pendulum

import (
	"math/big"

	"vsc-node/lib/intmath"
)

// SQ64 is a signed 8-decimal fixed-point number stored as int64.
// 1.0 == SQ64Scale; 0.5 == SQ64Scale/2; 0.0008 == 80_000.
//
// Range: representable values up to ~9.22e10 in real terms, which covers every
// pendulum ratio (s, w, multiplier) and small fractional rates by huge margin.
// Multiplications use big.Int internally to avoid intermediate overflow.
type SQ64 int64

// SQ64Scale is 10^8.
const SQ64Scale int64 = 100_000_000

// ProtocolFeeRateFixed is the SQ64 form of ProtocolFeeRate (8 bps).
const ProtocolFeeRateFixed SQ64 = 80_000

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

// CLPFeeInt returns the integer THORChain-style CLP fee:
//
//	CLP = floor( x² · Y / (x + X)² )
//
// Returns 0 for any non-positive input.
func CLPFeeInt(x, X, Y *big.Int) *big.Int {
	if x == nil || X == nil || Y == nil {
		return new(big.Int)
	}
	if x.Sign() <= 0 || X.Sign() <= 0 || Y.Sign() <= 0 {
		return new(big.Int)
	}
	xSquared := new(big.Int).Mul(x, x)
	sum := new(big.Int).Add(x, X)
	sumSquared := new(big.Int).Mul(sum, sum)
	return intmath.MulDivFloor(xSquared, Y, sumSquared)
}

// StabilizerParamsFixed is the SQ64 mirror of StabilizerParams.
// All fields use the same semantics as the float version.
type StabilizerParamsFixed struct {
	K    SQ64
	R0   SQ64
	Cap  SQ64
	Push SQ64
}

// DefaultStabilizerParamsFixed mirrors DefaultStabilizerParams (PDF defaults).
func DefaultStabilizerParamsFixed() StabilizerParamsFixed {
	return StabilizerParamsFixed{
		K:    SQ64(SQ64Scale),       // 1.0
		R0:   SQ64(SQ64Scale / 100), // 0.01
		Cap:  SQ64(SQ64Scale * 2),   // 2.0
		Push: SQ64(SQ64Scale),       // 1.0
	}
}

// StabilizerMultiplierFixed returns m(s, r) = 1 + K·|s−0.5|·(1 + r/R0)·push, capped (PDF §5).
//
// All arithmetic is integer fixed-point; the float StabilizerMultiplier exists
// only as a reference and for tests / dashboards.
func StabilizerMultiplierFixed(s, r SQ64, p StabilizerParamsFixed) SQ64 {
	r0 := p.R0
	if r0 <= 0 {
		// Match the float guard against divide-by-zero — pick a tiny positive.
		r0 = 1
	}

	// |s − 0.5|
	half := SQ64(SQ64Scale / 2)
	diff := SQ64Abs(s - half)

	// inner = 1 + r/r0
	rOverR0, _ := SQ64Div(r, r0) // r0 > 0 by construction
	inner := SQ64(SQ64Scale) + rOverR0

	// m = 1 + K·diff·inner·push
	tail := SQ64Mul(p.K, diff)
	tail = SQ64Mul(tail, inner)
	tail = SQ64Mul(tail, p.Push)
	m := SQ64(SQ64Scale) + tail

	if p.Cap > 0 && m > p.Cap {
		return p.Cap
	}
	return m
}

// ApplyMultiplierFixed returns floor( fee · m / SQ64Scale ).
// Used to apply the stabilizer multiplier to an integer base fee.
func ApplyMultiplierFixed(fee *big.Int, m SQ64) *big.Int {
	if fee == nil || fee.Sign() == 0 || m == 0 {
		return new(big.Int)
	}
	return intmath.MulDivFloor(fee, big.NewInt(int64(m)), sq64ScaleBig)
}
