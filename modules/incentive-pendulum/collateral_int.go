package pendulum

import (
	"math/big"

	"vsc-node/lib/intmath"
)

// CollateralReportBps is the integer collateral report. S is the V/E ratio in
// basis points (BpsScale = 1.0).
type CollateralReportBps struct {
	SBps int64

	UnderSecured bool
	IdealZone    bool
	SafeGrowth   bool
	WarningZone  bool
	ExtremeLow   bool
	ExtremeHigh  bool

	ProtocolRedirectRecommended bool
	ProtocolRedirectToNodes     bool
}

// RatioSVBps returns s = V/E expressed in basis points (HBD base units cancel
// in the ratio). Returns 0 if E <= 0 or V is nil/negative.
func RatioSVBps(V, E *big.Int) int64 {
	if V == nil || E == nil || E.Sign() <= 0 || V.Sign() < 0 {
		return 0
	}
	num := new(big.Int).Mul(V, big.NewInt(BpsScale))
	num.Quo(num, E)
	if !num.IsInt64() {
		return int64(^uint64(0) >> 1) // saturate — far above any meaningful s
	}
	return num.Int64()
}

// EffectiveBondHBDInt returns the integer effective bond in HBD base units.
//
//	E = floor( hiveStake_base · hivePriceHBDBps · effectiveFractionBps / BpsScale² )
//
// hiveStake is HIVE base units (precision 3). hivePriceHBDBps is HBD per HIVE
// in bps (BpsScale = 1.0). effectiveFractionBps is the [0, 1] effective-stake
// fraction in bps; values above BpsScale are clamped down. The HIVE/HBD base-
// unit precisions are equal, so they cancel and the result is in HBD base
// units directly.
func EffectiveBondHBDInt(hiveStake *big.Int, hivePriceHBDBps, effectiveFractionBps int64) *big.Int {
	if hiveStake == nil || hiveStake.Sign() <= 0 || hivePriceHBDBps <= 0 || effectiveFractionBps <= 0 {
		return new(big.Int)
	}
	frac := effectiveFractionBps
	if frac > BpsScale {
		frac = BpsScale
	}
	bpsScaleBig := big.NewInt(BpsScale)
	scaleSquared := new(big.Int).Mul(bpsScaleBig, bpsScaleBig)
	priceTimesFrac := new(big.Int).Mul(big.NewInt(hivePriceHBDBps), big.NewInt(frac))
	return intmath.MulDivFloor(hiveStake, priceTimesFrac, scaleSquared)
}

// CollateralFromSVBps classifies the V/E ratio s into collateral bands. The
// band edges are derived from the equilibrium target and the yield-ratio
// thresholds (see params.go) — they are level sets of the node/LP yield curve,
// naturally asymmetric around s_eq.
func CollateralFromSVBps(sBps int64) CollateralReportBps {
	r := CollateralReportBps{SBps: sBps}
	if sBps >= CliffSBps {
		r.UnderSecured = true
	}
	r.IdealZone = sBps >= idealLoEdgeBps && sBps <= idealHiEdgeBps
	r.SafeGrowth = sBps >= safeLoEdgeBps && sBps <= safeHiEdgeBps
	r.WarningZone = sBps > warnHiEdgeBps || sBps < warnLoEdgeBps
	r.ExtremeLow = sBps < extremeLoEdgeBps
	r.ExtremeHigh = sBps > extremeHiEdgeBps
	r.ProtocolRedirectRecommended = ProtocolFeeRedirectRecommendedBps(sBps)
	if r.ProtocolRedirectRecommended {
		r.ProtocolRedirectToNodes = ProtocolFeeRedirectToNodesBps(sBps)
	}
	return r
}

// ProtocolFeeRedirectRecommendedBps is the §9 trigger: true when s leaves the
// safe-growth band (s < RedirectLoBps or s > RedirectHiBps). The band is
// asymmetric around s_eq, so this is a two-sided bound, not a |s−s_eq| test.
func ProtocolFeeRedirectRecommendedBps(sBps int64) bool {
	return sBps < RedirectLoBps || sBps > RedirectHiBps
}

// ProtocolFeeRedirectToNodesBps is true on EXCESS liquidity (s > RedirectHiBps):
// route the protocol leg to nodes to shed liquidity. Below RedirectLoBps it is
// false (liquidity starved → fund LPs). Inspect only when
// ProtocolFeeRedirectRecommendedBps is true.
func ProtocolFeeRedirectToNodesBps(sBps int64) bool {
	return sBps > RedirectHiBps
}
