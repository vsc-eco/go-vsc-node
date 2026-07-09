package pendulum

import (
	"math/big"

	"vsc-node/lib/intmath"
)

// The pendulum's economic curve is fully determined by the equilibrium target
// s_eq plus a fixed set of node/LP yield-ratio band thresholds. Everything else
// — the hard cliff c and every collateral-band / §9-redirect s-edge — is
// DERIVED from these by inverting the yield-ratio curve ρ(s) = 2s²/(c−s). To
// retune the system, change TargetSBps (and the ratio thresholds, if desired)
// and rebuild; the cliff and all band edges move with it.
//
// These are consensus-critical: every node must compute identical splits, so
// they are compile-time constants, not runtime/governance config. (Runtime
// tunability would require activation-height gating.)
//
// Faithfulness check (see params_test.go): setting TargetSBps = 5_000 (s_eq =
// 0.5) reproduces the historical c = 1.0 cliff and the 0.30/0.70 safe band —
// the thresholds below are exactly the yield ratios the old fixed s-edges had.

// TargetSBps is the equilibrium collateralization ratio s_eq = V/E, in bps
// (BpsScale = 1.0). At s_eq the node and LP yields are equal. With E = ⅔·T this
// puts total-stake : liquidity at T/V = 1/(f·s_eq) = 1.5.
const TargetSBps int64 = BpsScale // s_eq = 1.0

// Yield-ratio band thresholds: the node/LP yield ratio ρ = 2s²/(c−s) at each
// zone boundary, in bps. These DEFINE the zones independently of s_eq; the
// s-edges are derived from them. Values reproduce the historical s-edges at
// s_eq = 0.5 (e.g. ρ=0.2571 ⇔ s=0.30 and ρ=3.2667 ⇔ s=0.70 when c=1).
const (
	idealRatioLoBps   int64 = 7_364   // 2·0.45²/0.55 → ideal lower edge
	idealRatioHiBps   int64 = 13_444  // 2·0.55²/0.45 → ideal upper edge
	safeRatioLoBps    int64 = 2_571   // 2·0.30²/0.70 → safe lower edge (= redirect lo)
	safeRatioHiBps    int64 = 32_667  // 2·0.70²/0.30 → safe upper edge (= redirect hi)
	warnRatioLoBps    int64 = 1_667   // 2·0.25²/0.75 → warning lower edge
	warnRatioHiBps    int64 = 45_000  // 2·0.75²/0.25 → warning upper edge
	extremeRatioLoBps int64 = 1_000   // 0.10 → extreme-low edge
	extremeRatioHiBps int64 = 100_000 // 10.0 → extreme-high edge
)

// CliffSBps is the hard-cliff ratio c = s_cliff, in bps. At or above it the
// vault is under-secured: 100% of the pendulum pot routes to nodes (LP share
// 0). It is forced by the equilibrium and the V = 2P geometry (w = ½):
//
//	c = s_eq²/w + s_eq = 2·s_eq² + s_eq
//
// At s_eq = 1.0 this is c = 3.0 (cliff at V ≥ 3E, i.e. T/V ≤ 0.5).
var CliffSBps = deriveCliffSBps(TargetSBps)

// Collateral-band s-edges (bps), derived by inverting the yield-ratio curve at
// the thresholds above. Naturally asymmetric around TargetSBps — tighter toward
// the floor (s=0), wider toward the distant cliff.
var (
	idealLoEdgeBps   = sEdgeForRatio(idealRatioLoBps, CliffSBps)
	idealHiEdgeBps   = sEdgeForRatio(idealRatioHiBps, CliffSBps)
	safeLoEdgeBps    = sEdgeForRatio(safeRatioLoBps, CliffSBps)
	safeHiEdgeBps    = sEdgeForRatio(safeRatioHiBps, CliffSBps)
	warnLoEdgeBps    = sEdgeForRatio(warnRatioLoBps, CliffSBps)
	warnHiEdgeBps    = sEdgeForRatio(warnRatioHiBps, CliffSBps)
	extremeLoEdgeBps = sEdgeForRatio(extremeRatioLoBps, CliffSBps)
	extremeHiEdgeBps = sEdgeForRatio(extremeRatioHiBps, CliffSBps)
)

// RedirectLoBps / RedirectHiBps are the §9 protocol-fee redirect thresholds.
// They coincide with the safe-growth band edges: the redirect activates exactly
// when s leaves the safe band. Below RedirectLo (liquidity starved) the protocol
// leg funds LPs to attract liquidity; above RedirectHi (excess liquidity) it
// funds nodes to shed it.
var (
	RedirectLoBps = safeLoEdgeBps
	RedirectHiBps = safeHiEdgeBps
)

// Geometry is the economic curve resolved for a specific chain height. Its root
// is the equilibrium target s_eq (TargetSBps); the hard cliff c and the §9
// redirect band derive from it. These are the consensus-critical inputs to the
// swap-fee accrual — the stabilizer fee multiplier (StabilizerMultiplierBps) and
// the node/LP split (wasm.splitFractionsBps) — that feed the pendulum:nodes
// bucket. A retune of these values therefore MUST be activation-height gated
// (GeometryAt) rather than a bare constant swap, or replaying pre-retune history
// accrues a different bucket and forks the settlement CID.
type Geometry struct {
	TargetSBps    int64
	CliffSBps     int64
	RedirectLoBps int64
	RedirectHiBps int64
}

// CliffTimesE returns c·E in HBD base units (floor) for this geometry's cliff —
// the per-geometry counterpart of the package CliffTimesE.
func (g Geometry) CliffTimesE(E *big.Int) *big.Int {
	return intmath.MulDivFloor(E, big.NewInt(g.CliffSBps), big.NewInt(BpsScale))
}

// geometryFrom derives the full curve from the equilibrium target s_eq (bps):
// cliff c = 2·s_eq² + s_eq, and the §9 redirect band = the safe-growth s-edges.
func geometryFrom(targetSBps int64) Geometry {
	cliff := deriveCliffSBps(targetSBps)
	return Geometry{
		TargetSBps:    targetSBps,
		CliffSBps:     cliff,
		RedirectLoBps: sEdgeForRatio(safeRatioLoBps, cliff),
		RedirectHiBps: sEdgeForRatio(safeRatioHiBps, cliff),
	}
}

// GeometryV1 is the ORIGINAL geometry (s_eq = 0.5, cliff c = 1.0, 0.30/0.70 safe
// band) — the curve in force before the params retune (a26684a4). GeometryV2 is
// the current retune (s_eq = 1.0, c = 3.0). By construction GeometryV2's fields
// equal the package-level TargetSBps / CliffSBps / RedirectLoBps / RedirectHiBps,
// so selecting GeometryV2 is byte-identical to the pre-gate (ungated) behaviour.
var (
	GeometryV1 = geometryFrom(5_000)
	GeometryV2 = geometryFrom(TargetSBps)
)

// GeometryAt resolves the geometry active at Hive L1 block height blockHeight.
// v2Height is the coordinated activation height of the retune
// (ConsensusParams.PendulumGeometryV2Height): below it the ORIGINAL GeometryV1
// curve is used so a full reindex reproduces pre-retune node-bucket accrual
// byte-for-byte; at/above it — and whenever v2Height == 0 (a chain that was
// always on the retuned curve, e.g. a fresh devnet) — the current GeometryV2
// curve. A pure function of height, so every node resolves the identical curve.
func GeometryAt(blockHeight, v2Height uint64) Geometry {
	if v2Height != 0 && blockHeight < v2Height {
		return GeometryV1
	}
	return GeometryV2
}

// deriveCliffSBps returns c = 2·s_eq² + s_eq in bps (w = ½ is structural, from
// the V = 2P geometry). With s in bps: 2·(S/B)²·B + S = 2·S²/B + S.
func deriveCliffSBps(targetSBps int64) int64 {
	twoSsq := new(big.Int).Mul(big.NewInt(targetSBps), big.NewInt(targetSBps))
	twoSsq.Mul(twoSsq, big.NewInt(2))
	twoSsq.Quo(twoSsq, big.NewInt(BpsScale))
	return twoSsq.Int64() + targetSBps
}

// sEdgeForRatio inverts the yield-ratio curve ρ(s) = 2s²/(c−s) to find the s
// (bps) at which the ratio equals ρ:
//
//	2s² + ρ·s − ρ·c = 0   ⇒   s = (−ρ + √(ρ² + 8·ρ·c)) / 4
//
// Carried in bps throughout (multiplying the real-unit equation by BpsScale²
// leaves 2S² + ρS − ρC = 0). Uses big.Int.Sqrt (exact integer floor) so the
// result is deterministic across nodes. ρ and c are positive, so the
// discriminant is positive and the positive root is the meaningful one.
func sEdgeForRatio(ratioBps, cliffSBps int64) int64 {
	rho := big.NewInt(ratioBps)
	disc := new(big.Int).Mul(rho, rho) // ρ²
	eightRhoC := new(big.Int).Mul(rho, big.NewInt(cliffSBps))
	eightRhoC.Mul(eightRhoC, big.NewInt(8)) // 8·ρ·c
	disc.Add(disc, eightRhoC)
	root := new(big.Int).Sqrt(disc) // floor(√disc)
	root.Sub(root, rho)             // −ρ + √disc
	root.Quo(root, big.NewInt(4))   // ÷4
	return root.Int64()
}

// CliffTimesE returns c·E in HBD base units (floor) — the threshold the vault
// liquidity V is compared against. V ≥ CliffTimesE(E) ⇒ under-secured cliff.
// Single source for the comparison so SplitInt and the wasm applier stay in
// lockstep for any c (integer or fractional).
func CliffTimesE(E *big.Int) *big.Int {
	return intmath.MulDivFloor(E, big.NewInt(CliffSBps), big.NewInt(BpsScale))
}

// YieldRatioBps returns the node/LP yield ratio 2s²/(c−s) at the given s, in
// bps; the band s-edges are level sets of this curve. Returns 0 outside the
// rewarded region (s ≤ 0 or s ≥ c, where LP share is 0).
func YieldRatioBps(sBps int64) int64 {
	if sBps <= 0 || sBps >= CliffSBps {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(sBps), big.NewInt(sBps))
	num.Mul(num, big.NewInt(2)) // 2S²
	num.Quo(num, big.NewInt(CliffSBps-sBps))
	return num.Int64()
}
