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

	ProtocolRedirectRecommended bool
	ProtocolRedirectToNodes     bool
}

// Band thresholds in bps (1.0 == 10_000).
const (
	bps_0_20 = int64(2_000)
	bps_0_25 = int64(2_500)
	bps_0_30 = int64(3_000)
	bps_0_45 = int64(4_500)
	bps_0_50 = int64(5_000)
	bps_0_55 = int64(5_500)
	bps_0_70 = int64(7_000)
	bps_0_75 = int64(7_500)
	bps_1_00 = int64(10_000)
)

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

// CollateralFromSVBps mirrors CollateralFromSV in basis-point form.
func CollateralFromSVBps(sBps int64) CollateralReportBps {
	r := CollateralReportBps{SBps: sBps}
	if sBps >= bps_1_00 {
		r.UnderSecured = true
	}
	r.IdealZone = sBps >= bps_0_45 && sBps <= bps_0_55
	r.SafeGrowth = sBps >= bps_0_30 && sBps <= bps_0_70
	r.WarningZone = sBps > bps_0_75 || sBps < bps_0_25
	r.ExtremeLow = sBps < bps_0_20
	r.ProtocolRedirectRecommended = ProtocolFeeRedirectRecommendedBps(sBps)
	if r.ProtocolRedirectRecommended {
		r.ProtocolRedirectToNodes = ProtocolFeeRedirectToNodesBps(sBps)
	}
	return r
}

// ProtocolFeeRedirectRecommendedBps is true when |s − 0.5| > 0.2 (in bps,
// a 2000-bps deviation from the equilibrium target).
func ProtocolFeeRedirectRecommendedBps(sBps int64) bool {
	d := sBps - bps_0_50
	if d < 0 {
		d = -d
	}
	return d > 2_000
}

// ProtocolFeeRedirectToNodesBps is true when s < 0.3 (starved nodes). Caller
// should only inspect this when ProtocolFeeRedirectRecommendedBps is true.
func ProtocolFeeRedirectToNodesBps(sBps int64) bool {
	return sBps < bps_0_30
}
