package pendulum

import "math"

// CollateralReport summarizes pendulum/collateral health for monitoring and DEX policy (PDF §11).
type CollateralReport struct {
	S float64 // V/E liquidity coverage ratio

	UnderSecured bool // s >= 1: hard cliff, pools receive 0% of R (PDF §6.1)
	IdealZone    bool // 0.45 <= s <= 0.55
	SafeGrowth   bool // 0.30 <= s <= 0.70
	WarningZone  bool // s > 0.75 || s < 0.25
	ExtremeLow   bool // s < 0.20

	ProtocolRedirectRecommended bool // |s-0.5| > 0.2 (PDF §9)
	ProtocolRedirectToNodes     bool // true if starved nodes (s < 0.3); false if starved pools (s > 0.7)
}

// CollateralFromSV computes bands from s = V/E alone (E > 0, V >= 0).
func CollateralFromSV(s float64) CollateralReport {
	r := CollateralReport{S: s}
	if s >= 1 {
		r.UnderSecured = true
	}
	r.IdealZone = s >= 0.45 && s <= 0.55
	r.SafeGrowth = s >= 0.30 && s <= 0.70
	r.WarningZone = s > 0.75 || s < 0.25
	r.ExtremeLow = s < 0.20
	r.ProtocolRedirectRecommended = ProtocolFeeRedirectRecommended(s)
	if r.ProtocolRedirectRecommended {
		r.ProtocolRedirectToNodes = ProtocolFeeRedirectToNodes(s)
	}
	return r
}

// EffectiveBondHBD maps validator HIVE (same units as price) to effective bond E in HBD-equivalent
// using the sole HIVE oracle price and an effective fraction of stake (PDF: bottom 2/3 → use 2/3).
func EffectiveBondHBD(hiveStake float64, hivePriceHBD float64, effectiveFraction float64) float64 {
	if hiveStake <= 0 || hivePriceHBD <= 0 || effectiveFraction <= 0 {
		return 0
	}
	if effectiveFraction > 1 {
		effectiveFraction = 1
	}
	return hiveStake * hivePriceHBD * effectiveFraction
}

// RatioSV returns s = V/E with guards (0 if E <= 0).
func RatioSV(V, E float64) float64 {
	if E <= 0 || math.IsNaN(E) || math.IsInf(E, 0) {
		return 0
	}
	return V / E
}
