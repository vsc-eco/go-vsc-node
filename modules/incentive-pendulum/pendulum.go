package pendulum

import "math"

// SplitInputs are E, T, V, P in one valuation unit (HBD minors treating HBD = $1; PDF “USD”).
// E = effective security bond, T = total effective bond, V = vault liquidity, P = pooled HBD liquidity.
type SplitInputs struct {
	R float64 // distributable CLP fees for this epoch
	E float64
	T float64
	V float64
	P float64
	U float64 // T/E ratio when computed off-chain; typically 1.5. If 0, treated as T/E when E > 0.
}

// SplitOutputs are closed-form shares and yields (PDF §6).
type SplitOutputs struct {
	S float64 // V/E
	W float64 // P/V, 0 if V <= 0

	FinalNodeShare float64
	FinalPoolShare float64

	NodeYield    float64 // per PDF: sR/(E·denom) when s < 1; R/T when s >= 1
	PoolYield    float64 // (1−s)R/(2sE·denom) when s < 1; 0 when s >= 1
	UnderSecured bool    // s >= 1 hard cliff
}

// Split computes pendulum allocation (PDF §6). Returns ok false if E <= 0 or T <= 0.
func Split(in SplitInputs) (out SplitOutputs, ok bool) {
	out = SplitOutputs{}
	if in.E <= 0 || in.T <= 0 {
		return out, false
	}
	u := in.U
	if u <= 0 {
		u = in.T / in.E
	}

	out.S = in.V / in.E
	if in.V > 0 {
		out.W = in.P / in.V
	}

	if out.S >= 1 {
		out.UnderSecured = true
		out.FinalNodeShare = in.R
		out.FinalPoolShare = 0
		out.NodeYield = in.R / in.T
		out.PoolYield = 0
		return out, true
	}

	denom := u*out.S + out.W*(1-out.S)
	if denom <= 0 || math.IsNaN(denom) {
		out.FinalNodeShare = in.R
		out.FinalPoolShare = 0
		out.NodeYield = in.R / in.T
		out.PoolYield = 0
		return out, true
	}

	out.FinalNodeShare = in.R * (u * out.S) / denom
	out.FinalPoolShare = in.R * (out.W * (1 - out.S)) / denom
	out.NodeYield = (out.S * in.R) / (in.E * denom)
	if out.S > 0 {
		out.PoolYield = ((1 - out.S) * in.R) / (2 * out.S * in.E * denom)
	}
	return out, true
}

// YieldRatio returns nodeYield/poolYield when s < 1 and poolYield > 0; otherwise +Inf if pool zero.
// PDF §7: ratio = 2s²/(1−s) (independent of u, w, R, E).
func YieldRatio(s float64) float64 {
	if s >= 1 || s <= 0 {
		return math.Inf(1)
	}
	return (2 * s * s) / (1 - s)
}
