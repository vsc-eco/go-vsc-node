package pendulum

import "math"

// ProtocolFeeRate is the fixed protocol fee as a fraction of notional (8 bps).
const ProtocolFeeRate = 0.0008

// CLPFee returns the THORChain-style continuous liquidity pool fee for a swap:
//
//	CLP = x² Y / (x + X)²
//
// x is swap input size, X is pool depth of the input asset, Y is pool depth of the output asset.
// All quantities must use the same unit (e.g. HBD minor units as float64).
func CLPFee(x, X, Y float64) float64 {
	if x <= 0 || X <= 0 || Y <= 0 {
		return 0
	}
	d := x + X
	return (x * x * Y) / (d * d)
}

// PendulumFeeFraction returns the fraction of total swap fees (protocol + CLP) that flows into
// the pendulum pool R when expressed relative to trade size r = swap_value / pool_side_value (PDF §3).
//
//	CLP / Total = r / (r + 0.0008·(1+r)²)
func PendulumFeeFraction(r float64) float64 {
	if r <= 0 {
		return 0
	}
	denom := r + ProtocolFeeRate*math.Pow(1+r, 2)
	return r / denom
}

// StabilizerParams holds governance-tunable defaults from PDF §5.
type StabilizerParams struct {
	K    float64 // e.g. 1.0
	R0   float64 // e.g. 0.01 (1% of pool)
	Cap  float64 // e.g. 2.0
	Push float64 // 1.0 if trade exacerbates imbalance, 0.7 if corrective
}

// DefaultStabilizerParams matches PDF recommended defaults (before cap).
func DefaultStabilizerParams() StabilizerParams {
	return StabilizerParams{K: 1.0, R0: 0.01, Cap: 2.0, Push: 1.0}
}

// StabilizerMultiplier returns m(s, r) = 1 + k·|s−0.5|·(1 + r/r0)·push, capped (PDF §5).
// Total fee paid = (protocol + CLP) × m.
func StabilizerMultiplier(s, r float64, p StabilizerParams) float64 {
	if p.R0 <= 0 {
		p.R0 = 1e-12
	}
	inner := 1 + r/p.R0
	m := 1 + p.K*math.Abs(s-0.5)*inner*p.Push
	if p.Cap > 0 && m > p.Cap {
		return p.Cap
	}
	return m
}

// ProtocolFeeRedirectRecommended is true when |s−0.5| > 0.2 (PDF §9): redirect protocol fee
// portion of that epoch to the starved side (nodes if s < 0.3, pools if s > 0.7).
func ProtocolFeeRedirectRecommended(s float64) bool {
	return math.Abs(s-0.5) > 0.2
}

// ProtocolFeeRedirectToNodes is true when s < 0.3 (starved nodes); false when s > 0.7 (starved pools).
// If ProtocolFeeRedirectRecommended(s) is false, the return value is meaningless.
func ProtocolFeeRedirectToNodes(s float64) bool {
	return s < 0.3
}
