package pendulum

import (
	"math/big"

	"vsc-node/lib/intmath"
)

// SplitInputsInt is the integer / base-unit form of SplitInputs. All values are
// non-negative; the math path uses base units (HBD with 3 decimals) treating
// HBD = $1. The U field from the float SplitInputs is not needed: the rearranged
// closed form below uses T, E, V, P, R directly.
type SplitInputsInt struct {
	R *big.Int // distributable CLP fees this epoch
	E *big.Int // effective security bond
	T *big.Int // total effective bond
	V *big.Int // vault liquidity (≈ 2·Σ pool HBD-side depth)
	P *big.Int // pooled HBD liquidity (Σ pool HBD-side depth)
}

// SplitOutputsInt is the integer split. FinalNodeShare + FinalPoolShare == R
// exactly; any base-unit residual from floor division is assigned to the node
// side per the plan.
type SplitOutputsInt struct {
	FinalNodeShare *big.Int
	FinalPoolShare *big.Int
	UnderSecured   bool // s >= 1 (V >= E)
}

// SplitInt is the integer-precision Split. It returns ok=false on invalid
// inputs (nil, negative, or E/T <= 0). For V == 0 or V >= E it falls back to
// 100% nodes (matching the float Split behaviour: cliff and degenerate-vault
// branches both route everything to nodes). Otherwise it uses the closed form
//
//	denom         = T·V² + P·E·(E − V)
//	FinalPoolShare = floor( R · P·E·(E − V) / denom )
//	FinalNodeShare = R − FinalPoolShare        (residual lands on node side)
//
// which is algebraically equivalent to the PDF formulas after clearing the
// E²·V denominator.
func SplitInt(in SplitInputsInt) (SplitOutputsInt, bool) {
	out := SplitOutputsInt{}

	if in.R == nil || in.E == nil || in.T == nil || in.V == nil || in.P == nil {
		return out, false
	}
	if in.E.Sign() <= 0 || in.T.Sign() <= 0 {
		return out, false
	}
	if in.R.Sign() < 0 || in.V.Sign() < 0 || in.P.Sign() < 0 {
		return out, false
	}

	// Hard cliff: V >= E.
	if in.V.Cmp(in.E) >= 0 {
		out.UnderSecured = true
		out.FinalNodeShare = new(big.Int).Set(in.R)
		out.FinalPoolShare = new(big.Int)
		return out, true
	}

	// Degenerate vault — match float fallback (denom = 0 in the original formula).
	if in.V.Sign() == 0 {
		out.FinalNodeShare = new(big.Int).Set(in.R)
		out.FinalPoolShare = new(big.Int)
		return out, true
	}

	// denom = T·V² + P·E·(E − V).  V > 0 and V < E here, so both terms are >= 0
	// and denom is strictly positive once T > 0.
	tvSquared := new(big.Int).Mul(in.V, in.V)
	tvSquared.Mul(tvSquared, in.T) // T·V²

	eMinusV := new(big.Int).Sub(in.E, in.V) // E − V > 0
	peTerm := new(big.Int).Mul(in.P, in.E)
	peTerm.Mul(peTerm, eMinusV) // P·E·(E−V) >= 0

	denom := new(big.Int).Add(tvSquared, peTerm)
	if denom.Sign() == 0 {
		// Only reachable when T·V² == 0 (impossible given T,V > 0) and P·E·(E−V) == 0.
		// Defensive fallback — route to nodes.
		out.FinalNodeShare = new(big.Int).Set(in.R)
		out.FinalPoolShare = new(big.Int)
		return out, true
	}

	// pool_share = floor( R · P·E·(E−V) / denom )
	poolShare := intmath.MulDivFloor(in.R, peTerm, denom)
	// node_share = R − pool_share — gives the [0, 1) residual to the node side.
	nodeShare := new(big.Int).Sub(in.R, poolShare)

	out.FinalNodeShare = nodeShare
	out.FinalPoolShare = poolShare
	return out, true
}
