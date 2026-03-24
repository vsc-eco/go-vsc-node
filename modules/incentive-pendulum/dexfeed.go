package pendulum

import "math"

// PoolPendulumLiquidity is HBD-side depth for one approved CLP pool (HackMD single-side tracking).
// V_contrib for global V is typically 2*PHbd (symmetry assumption); P_contrib = PHbd.
type PoolPendulumLiquidity struct {
	PoolID string
	PHbd   float64 // HBD-side depth (HBD = $1 units)
}

// SumPendulumVault aggregates vault metrics for pendulum s = V/E.
// V_total = sum(2*PHbd), P_total = sum(PHbd) per approved pools.
func SumPendulumVault(pools []PoolPendulumLiquidity) (vTotal, pTotal float64) {
	for _, p := range pools {
		if p.PHbd <= 0 || math.IsNaN(p.PHbd) {
			continue
		}
		pTotal += p.PHbd
		vTotal += 2 * p.PHbd
	}
	return vTotal, pTotal
}

// SwapLegDepths are CLP reserves for the swap fee formula (open-market; no extra oracles).
type SwapLegDepths struct {
	X float64 // input-side depth
	Y float64 // output-side depth
}

// SwapFeeQuote is what a DEX/router applies for one swap (PDF §2–§5, §9 surplus semantics).
// User pays ChargedTotal. BaseCLP accrues to pendulum R; stabilizer surplus is redirect policy.
type SwapFeeQuote struct {
	RTrade float64 // swap_value / pool_side_value (input leg: x/X)

	BaseProtocol float64 // 8 bps on swap notional (input size x)
	BaseCLP      float64 // x²Y/(x+X)²
	BaseSubtotal float64 // BaseProtocol + BaseCLP

	Multiplier   float64 // m(s, r)
	ChargedTotal float64 // BaseSubtotal * Multiplier

	// AccrueToPendulumR is the CLP leg that funds R (PDF: only CLP flows into R), pre-multiplier.
	AccrueToPendulumR float64
	// StabilizerSurplus is (ChargedTotal - BaseSubtotal); redirect per PDF §9 / governance.
	StabilizerSurplus float64

	// PendulumFractionOfTotalFees is CLP/Total for the idealized fee mix at this r (PDF §3), not user charge.
	PendulumFractionOfTotalFees float64

	Collateral CollateralReport
}

// QuoteSwapFees builds fee lines for a single swap given global s and trade-relative r.
// exacerbatesImbalance sets stabilizer push to 1.0 vs 0.7 (PDF §5).
// swapNotional should match the protocol fee base (use input amount x in same units as X,Y).
func QuoteSwapFees(
	swapInputX float64,
	depths SwapLegDepths,
	globalS float64,
	exacerbatesImbalance bool,
	stab StabilizerParams,
) (q SwapFeeQuote) {
	X, Y := depths.X, depths.Y
	if swapInputX <= 0 || X <= 0 || Y <= 0 {
		q.Collateral = CollateralFromSV(globalS)
		return q
	}

	rTrade := swapInputX / X
	q.RTrade = rTrade
	q.BaseCLP = CLPFee(swapInputX, X, Y)
	q.BaseProtocol = swapInputX * ProtocolFeeRate
	q.BaseSubtotal = q.BaseProtocol + q.BaseCLP

	push := stab.Push
	if stab.Push <= 0 {
		push = DefaultStabilizerParams().Push
	}
	if !exacerbatesImbalance {
		push = 0.7
	}
	sp := stab
	sp.Push = push
	q.Multiplier = StabilizerMultiplier(globalS, rTrade, sp)
	q.ChargedTotal = q.BaseSubtotal * q.Multiplier
	q.AccrueToPendulumR = q.BaseCLP
	q.StabilizerSurplus = q.ChargedTotal - q.BaseSubtotal
	q.PendulumFractionOfTotalFees = PendulumFeeFraction(rTrade)
	q.Collateral = CollateralFromSV(globalS)
	return q
}

// PendulumBolt bundles config for a bolt-on integration (DEX + settlement callers).
type PendulumBolt struct {
	Stabilizer StabilizerParams
	// EffectiveStakeFraction is applied to HIVE stake for E (e.g. 2/3).
	EffectiveStakeFraction float64
}

// NewPendulumBolt returns defaults suitable for Magi PDF.
func NewPendulumBolt() *PendulumBolt {
	return &PendulumBolt{
		Stabilizer:             DefaultStabilizerParams(),
		EffectiveStakeFraction: 2.0 / 3.0,
	}
}

// NetworkSnapshot is minimal state to compute s and a pendulum split (HBD = $1 units).
type NetworkSnapshot struct {
	TotalHiveStake float64 // raw consensus HIVE (pre-fraction)
	HivePriceHBD   float64 // sole oracle output; HIVE priced in HBD
	TotalBondT     float64 // T in HBD-equivalent (if 0, derived as hive*T/E ratio not available—caller supplies T)
	Pools          []PoolPendulumLiquidity
}

// Evaluate computes E from stake×price×fraction, V/P from pools, s, collateral report, and optional split for epoch R.
func (b *PendulumBolt) Evaluate(net NetworkSnapshot, R float64) (BoltEvaluation, bool) {
	ev := BoltEvaluation{}
	if b == nil {
		b = NewPendulumBolt()
	}
	frac := b.EffectiveStakeFraction
	if frac <= 0 {
		frac = 2.0 / 3.0
	}
	E := EffectiveBondHBD(net.TotalHiveStake, net.HivePriceHBD, frac)
	V, P := SumPendulumVault(net.Pools)
	ev.E, ev.T, ev.V, ev.P = E, net.TotalBondT, V, P
	ev.S = RatioSV(V, E)
	ev.W = 0
	if V > 0 {
		ev.W = P / V
	}
	ev.Collateral = CollateralFromSV(ev.S)

	T := net.TotalBondT
	if T <= 0 && E > 0 {
		// Without T, cannot compute yields; still return geometry.
		return ev, false
	}

	split, ok := Split(SplitInputs{R: R, E: E, T: T, V: V, P: P, U: 0})
	if ok {
		ev.Split = split
	}
	return ev, ok
}

// BoltEvaluation is the feed DEX/indexer/settlement use each tick.
type BoltEvaluation struct {
	E, T, V, P, S, W float64
	Collateral       CollateralReport
	Split            SplitOutputs
}

// QuoteSwap is convenience on the bolt with configured stabilizer.
func (b *PendulumBolt) QuoteSwap(x float64, depths SwapLegDepths, globalS float64, exacerbates bool) SwapFeeQuote {
	if b == nil {
		b = NewPendulumBolt()
	}
	return QuoteSwapFees(x, depths, globalS, exacerbates, b.Stabilizer)
}
