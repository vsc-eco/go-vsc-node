package pendulum

import (
	"math/big"

	"vsc-node/lib/intmath"
)

// CollateralReportFixed mirrors CollateralReport with S in SQ64 instead of float64.
type CollateralReportFixed struct {
	S SQ64

	UnderSecured bool
	IdealZone    bool
	SafeGrowth   bool
	WarningZone  bool
	ExtremeLow   bool

	ProtocolRedirectRecommended bool
	ProtocolRedirectToNodes     bool
}

// Band thresholds in SQ64.
var (
	sq64_0_20 = SQ64(20 * SQ64Scale / 100)
	sq64_0_25 = SQ64(25 * SQ64Scale / 100)
	sq64_0_30 = SQ64(30 * SQ64Scale / 100)
	sq64_0_45 = SQ64(45 * SQ64Scale / 100)
	sq64_0_50 = SQ64(50 * SQ64Scale / 100)
	sq64_0_55 = SQ64(55 * SQ64Scale / 100)
	sq64_0_70 = SQ64(70 * SQ64Scale / 100)
	sq64_0_75 = SQ64(75 * SQ64Scale / 100)
	sq64_1_00 = SQ64(SQ64Scale)
)

// RatioSVFixed returns s = V/E in SQ64 (HBD base units cancel in the ratio).
// Returns 0 if E <= 0 or either input is nil.
func RatioSVFixed(V, E *big.Int) SQ64 {
	if V == nil || E == nil || E.Sign() <= 0 || V.Sign() < 0 {
		return 0
	}
	// s = V/E; in SQ64 = floor(V·SQ64Scale / E)
	num := new(big.Int).Mul(V, sq64ScaleBig)
	num.Quo(num, E)
	if !num.IsInt64() {
		// Saturate to max int64 — far above any meaningful s.
		return SQ64(int64(^uint64(0) >> 1))
	}
	return SQ64(num.Int64())
}

// EffectiveBondHBDInt returns the integer effective bond in HBD base units.
//
//	E = floor( hiveStake_base · hivePriceHBD · effectiveFraction / SQ64Scale² )
//
// `hiveStake` is in HIVE base units (3-decimal Hive). `hivePriceHBD` is the
// HBD-per-HIVE price as SQ64 (decimal scales of HIVE and HBD cancel since both
// are 3-decimal). `effectiveFraction` is clamped to [0, 1].
func EffectiveBondHBDInt(hiveStake *big.Int, hivePriceHBD SQ64, effectiveFraction SQ64) *big.Int {
	if hiveStake == nil || hiveStake.Sign() <= 0 || hivePriceHBD <= 0 || effectiveFraction <= 0 {
		return new(big.Int)
	}
	frac := effectiveFraction
	if frac > SQ64(SQ64Scale) {
		frac = SQ64(SQ64Scale)
	}
	// scaleSquared = SQ64Scale² to divide out the two SQ64 multiplicands.
	scaleSquared := new(big.Int).Mul(sq64ScaleBig, sq64ScaleBig)
	priceTimesFrac := new(big.Int).Mul(big.NewInt(int64(hivePriceHBD)), big.NewInt(int64(frac)))
	return intmath.MulDivFloor(hiveStake, priceTimesFrac, scaleSquared)
}

// CollateralFromSVFixed mirrors CollateralFromSV in fixed-point.
func CollateralFromSVFixed(s SQ64) CollateralReportFixed {
	r := CollateralReportFixed{S: s}
	if s >= sq64_1_00 {
		r.UnderSecured = true
	}
	r.IdealZone = s >= sq64_0_45 && s <= sq64_0_55
	r.SafeGrowth = s >= sq64_0_30 && s <= sq64_0_70
	r.WarningZone = s > sq64_0_75 || s < sq64_0_25
	r.ExtremeLow = s < sq64_0_20
	r.ProtocolRedirectRecommended = ProtocolFeeRedirectRecommendedFixed(s)
	if r.ProtocolRedirectRecommended {
		r.ProtocolRedirectToNodes = ProtocolFeeRedirectToNodesFixed(s)
	}
	return r
}

// ProtocolFeeRedirectRecommendedFixed is true when |s−0.5| > 0.2.
func ProtocolFeeRedirectRecommendedFixed(s SQ64) bool {
	return SQ64Abs(s-sq64_0_50) > SQ64(20*SQ64Scale/100)
}

// ProtocolFeeRedirectToNodesFixed is true when s < 0.3 (starved nodes).
// Caller should only inspect this when ProtocolFeeRedirectRecommendedFixed is true.
func ProtocolFeeRedirectToNodesFixed(s SQ64) bool {
	return s < sq64_0_30
}
