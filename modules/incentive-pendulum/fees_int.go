package pendulum

import (
	"math/big"

	"vsc-node/lib/intmath"
)

// BpsScale is re-exported from lib/intmath so callers don't need a second
// import for the basis-point denominator. 1.0 == 10_000 bps.
const BpsScale = intmath.BpsScale

// ProtocolFeeRateBps is the per-swap protocol fee rate. 8 bps = 0.08% of the
// gross output asset. Matches the existing contract math (`baseFee = grossOut
// * feeBps / 10000`) bit-for-bit, with no SQ64 intermediary.
const ProtocolFeeRateBps int64 = 8

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

// StabilizerParamsBps holds the stabilizer-multiplier governance parameters in
// basis points. K, R0, Cap, and Push all use the BpsScale denominator.
//
// Precision floor: 1 bps = 0.01% on each parameter. R0 below 1 bps is clamped
// up to 1 bps to avoid divide-by-zero — matches the float guard's intent.
type StabilizerParamsBps struct {
	KBps    int64 // stabilizer slope; default 1.0 → 10000
	R0Bps   int64 // r-normalization knee; default 0.01 → 100
	CapBps  int64 // multiplier upper cap; default 2.0 → 20000
	PushBps int64 // mode-dependent push factor; default 1.0 → 10000
}

// DefaultStabilizerParamsBps mirrors the prior PDF defaults.
func DefaultStabilizerParamsBps() StabilizerParamsBps {
	return StabilizerParamsBps{
		KBps:    BpsScale,
		R0Bps:   BpsScale / 100,
		CapBps:  BpsScale * 2,
		PushBps: BpsScale,
	}
}

// StabilizerMultiplierBps returns m(s, r) = 1 + K · |s − 0.5| · (1 + r/R0) · push,
// capped at p.CapBps. All inputs and the result use the bps scale.
//
// Each chained product is a MulDivFloor through BpsScale, so the multiplier
// accumulates at most ~3 bps of integer-floor rounding. The cap clamps the
// runaway tail before it leaves the function.
func StabilizerMultiplierBps(sBps, rBps int64, p StabilizerParamsBps) int64 {
	r0 := p.R0Bps
	if r0 <= 0 {
		r0 = 1
	}

	half := BpsScale / 2
	diff := sBps - half
	if diff < 0 {
		diff = -diff
	}

	// inner = 1 + r/R0, in bps.
	rOverR0 := intmath.MulDivFloorI64(rBps, BpsScale, r0)
	inner := BpsScale + rOverR0

	tail := intmath.MulDivFloorI64(p.KBps, diff, BpsScale)
	tail = intmath.MulDivFloorI64(tail, inner, BpsScale)
	tail = intmath.MulDivFloorI64(tail, p.PushBps, BpsScale)

	m := BpsScale + tail
	if p.CapBps > 0 && m > p.CapBps {
		return p.CapBps
	}
	return m
}

// ApplyMultiplierBps returns floor(fee · m / BpsScale). Used to apply the
// stabilizer multiplier to an integer base fee in HBD base units.
func ApplyMultiplierBps(fee *big.Int, mBps int64) *big.Int {
	if fee == nil || fee.Sign() == 0 || mBps == 0 {
		return new(big.Int)
	}
	return intmath.MulDivFloor(fee, big.NewInt(mBps), big.NewInt(BpsScale))
}
