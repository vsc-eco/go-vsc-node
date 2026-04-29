package pendulum

import (
	"math/big"

	"vsc-node/lib/intmath"
)

// SQ64 is re-exported from lib/intmath so callers can keep using
// pendulum.SQ64 / pendulum.SQ64FromFloat / etc. unchanged.
type SQ64 = intmath.SQ64

// SQ64Scale is 10^8 (re-exported).
const SQ64Scale = intmath.SQ64Scale

// ProtocolFeeRateFixed is the SQ64 form of ProtocolFeeRate (8 bps).
const ProtocolFeeRateFixed SQ64 = 80_000

var sq64ScaleBig = big.NewInt(SQ64Scale)

// SQ64FromFloat re-exports intmath.SQ64FromFloat.
func SQ64FromFloat(f float64) SQ64 { return intmath.SQ64FromFloat(f) }

// SQ64Mul re-exports intmath.SQ64Mul.
func SQ64Mul(a, b SQ64) SQ64 { return intmath.SQ64Mul(a, b) }

// SQ64Div re-exports intmath.SQ64Div.
func SQ64Div(a, b SQ64) (SQ64, bool) { return intmath.SQ64Div(a, b) }

// SQ64Abs re-exports intmath.SQ64Abs.
func SQ64Abs(a SQ64) SQ64 { return intmath.SQ64Abs(a) }

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
