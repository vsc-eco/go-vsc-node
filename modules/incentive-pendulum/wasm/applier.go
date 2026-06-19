// Package pendulumwasm wires the swap-time pendulum SDK method
// (system.pendulum_apply_swap_fees) to its dependencies. The applier
// owns the math + accrual side-effect; the SDK package and the
// wasm execution context only see a result-typed interface from
// modules/wasm/context.
package pendulumwasm

import (
	"errors"
	"fmt"
	"math/big"

	"vsc-node/lib/intmath"
	"vsc-node/modules/db/vsc/contracts"
	pendulum "vsc-node/modules/incentive-pendulum"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	wasm_context "vsc-node/modules/wasm/context"

	"github.com/JustinKnueppel/go-result"
)

// GeometryReader returns the live (V, P, E, T, sBps) geometry for a swap at
// the given block height. Production wires the GeometryComputer + FeedTracker
// directly; tests stub with a static value. The boolean follows
// `GeometryOutputs.OK` semantics — if false the applier rejects the swap with
// `errSnapshotUnavailable` (the geometry isn't computable yet, e.g. pre-W7
// warmup before bond data exists).
type GeometryReader interface {
	GeometryAt(blockHeight uint64) (pendulumoracle.GeometryOutputs, bool)
}

// WhitelistGetter returns the current pool whitelist (returned slice may be a
// fresh copy; the applier reads it through this hook so live config changes
// are picked up without re-construction).
type WhitelistGetter func() []string

// Config is the per-network tuning the applier reads on every swap.
type Config struct {
	Stabilizer      pendulum.StabilizerParamsBps
	NetworkShareNum int64 // 1 of 4 → 25% network share
	NetworkShareDen int64
	// MinFractionBps is the LP-side floor the plan parks behind a TODO. Zero
	// means PDF behavior (no floor); leaving as a Config knob so we can dial
	// it without code change once the team picks a value.
	MinFractionBps int
}

// DefaultConfig returns the v1 testnet defaults: PDF stabilizer, 25% network
// share, no LP floor. Override per-network via SystemConfig.
func DefaultConfig() Config {
	return Config{
		Stabilizer:      pendulum.DefaultStabilizerParamsBps(),
		NetworkShareNum: 1,
		NetworkShareDen: 4,
	}
}

// Applier is the concrete PendulumApplier impl wired by the state engine.
// It is stateless across calls — per-call ledger movement happens through the
// AccrueNodeBucketFn the execution context supplies at call time.
type Applier struct {
	geometry  GeometryReader
	whitelist WhitelistGetter
	cfg       Config
}

// New constructs an Applier. Any nil dep produces an applier that always
// returns ErrUnimplemented from ApplySwapFees, so the wasm runtime can call
// without worrying about partial wiring.
func New(geometry GeometryReader, whitelist WhitelistGetter, cfg Config) *Applier {
	return &Applier{geometry: geometry, whitelist: whitelist, cfg: cfg}
}

// Sentinel errors. Wrapped with contracts.SDK_ERROR so the wasm runtime maps
// them to the standard sdk-error code.
var (
	errNotWhitelisted       = errors.New("contract not whitelisted")
	errSnapshotUnavailable  = errors.New("pendulum snapshot unavailable")
	errInvalidArgument      = errors.New("invalid pendulum swap argument")
	errInsufficientReserves = errors.New("insufficient reserves for pendulum split")
	errAccrualFailed        = errors.New("pendulum accrual failed")
	errArithmeticOverflow   = errors.New("pendulum arithmetic overflow")
)

func sdkErr[T any](inner error) result.Result[T] {
	return result.Err[T](errors.Join(fmt.Errorf(contracts.SDK_ERROR), inner))
}

// ApplySwapFees implements wasm_context.PendulumApplier under the unified
// output-side fee model — both protocol and CLP fees live in the output
// asset, matching the existing pre-pendulum contract math. The contract
// passes only (assetIn, assetOut, x, X, Y, exacerbates); the SDK derives
// gross output, base CLP, base protocol, the stabilizer surplus, the 25%
// network cut, the pendulum split, and any non-HBD-leg conversion.
//
// Reserve flow:
//
//	newX = X + x                                (entire input enters the pool)
//	newY = Y - userOutput                       (the user takes only userOutput)
//	                                             — all fees stay in pool reserves
//	then if assetOut == "hbd":
//	  newY -= nodeShareOutput                   (HBD leaves pool to nodes bucket)
//	else (assetOut is non-HBD):
//	  hbdOut = nodeShareOutput · newX / (newY + nodeShareOutput)
//	  newY += nodeShareOutput                   (virtual: non-HBD added back)
//	  newX -= hbdOut                            (HBD leaves X reserve to nodes bucket)
func (a *Applier) ApplySwapFees(
	contractID, txID string,
	blockHeight uint64,
	args wasm_context.PendulumSwapFeeArgs,
	accrueNodeBucket wasm_context.AccrueNodeBucketFn,
) result.Result[wasm_context.PendulumSwapFeeResult] {
	if a == nil || a.geometry == nil || a.whitelist == nil || accrueNodeBucket == nil {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errors.New("pendulum applier not configured"))
	}

	// 1. Whitelist check.
	if !contractWhitelisted(contractID, a.whitelist()) {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errNotWhitelisted)
	}

	// Validate inputs.
	if args.X <= 0 || args.XReserve <= 0 || args.YReserve <= 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInvalidArgument)
	}
	assetIn := normalizeAsset(args.AssetIn)
	assetOut := normalizeAsset(args.AssetOut)
	if assetIn == "" || assetOut == "" || assetIn == assetOut {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInvalidArgument)
	}
	if assetIn != "hbd" && assetOut != "hbd" {
		// Testnet rollout is HBD-paired only.
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInvalidArgument)
	}

	// 2. Read live geometry. Recomputed at the swap's block height from
	// on-chain inputs (committee bond + whitelisted pool reserves) — no
	// per-tick snapshot cache.
	geo, ok := a.geometry.GeometryAt(blockHeight)
	if !ok || !geo.OK {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errSnapshotUnavailable)
	}
	V := big.NewInt(geo.V)
	E := big.NewInt(geo.E)
	P := big.NewInt(geo.P)
	T := big.NewInt(geo.T)
	sBps := geo.SBps
	if E.Sign() <= 0 || T.Sign() <= 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errSnapshotUnavailable)
	}

	xBig := big.NewInt(args.X)
	xReserveBig := big.NewInt(args.XReserve)
	yReserveBig := big.NewInt(args.YReserve)

	// 3. Output-side gross + base fees.
	xPlusReserve := new(big.Int).Add(xReserveBig, xBig)
	if xPlusReserve.Sign() == 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInvalidArgument)
	}
	grossOut := intmath.MulDivFloor(xBig, yReserveBig, xPlusReserve)
	if grossOut.Sign() <= 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}
	if grossOut.Cmp(yReserveBig) > 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}
	// CLP = x²·Y / (x+X)²  (output units)
	xSquaredY := new(big.Int).Mul(xBig, xBig)
	denomSquared := new(big.Int).Mul(xPlusReserve, xPlusReserve)
	baseCLP := intmath.MulDivFloor(xSquaredY, yReserveBig, denomSquared)
	// Protocol fee = grossOut · 8 bps  (output units; matches the contract's
	// existing `baseFee = grossOut * feeBps / 10000`).
	baseProtocol := intmath.MulDivFloor(
		grossOut,
		big.NewInt(pendulum.ProtocolFeeRateBps),
		big.NewInt(pendulum.BpsScale),
	)

	// 4. Stabilizer multiplier on both fee legs. PDF §5: total fee is
	// (baseProtocol + baseCLP) · m; the surplus on each leg funds the
	// rebalancing incentive and flows through the same 25%/75%
	// network/pendulum split as the base fee.
	//
	// "Exacerbates" is derived from snapshot s and the swap direction — the
	// SDK has all inputs, so accepting a contract-supplied hint would be a
	// pure non-determinism vector with no information gain. HBD-in raises
	// P (and therefore s = V/E = 2P/E); HBD-out lowers it. A swap is
	// corrective when its direction matches the move toward the target s_eq;
	// any move away (including any move at exactly s = s_eq) is exacerbating.
	rTradeBps, err := tradeRatioBps(args.X, args.XReserve)
	if err != nil {
		return sdkErr[wasm_context.PendulumSwapFeeResult](err)
	}
	stab := a.cfg.Stabilizer
	if !exacerbatesFromSnapshot(sBps, assetIn == "hbd") {
		// PDF §5: non-exacerbating swaps push at 0.7×.
		stab.PushBps = pendulum.BpsScale * 70 / 100
	}
	multiplierBps, err := pendulum.StabilizerMultiplierBps(sBps, rTradeBps, stab)
	if err != nil {
		return sdkErr[wasm_context.PendulumSwapFeeResult](err)
	}
	chargedCLP := pendulum.ApplyMultiplierBps(baseCLP, multiplierBps)
	chargedProtocol := pendulum.ApplyMultiplierBps(baseProtocol, multiplierBps)

	// 5. Total fees per type, all output units. m rides on both legs;
	// totalX == chargedX == baseX·m (with a floor of baseX so a degenerate
	// m < 1 cannot subtract from the base fee).
	totalCLP := chargedCLP
	if totalCLP.Cmp(baseCLP) < 0 {
		totalCLP = baseCLP
	}
	totalProtocol := chargedProtocol
	if totalProtocol.Cmp(baseProtocol) < 0 {
		totalProtocol = baseProtocol
	}

	// 6. User pays both fees out of grossOut.
	totalFee := new(big.Int).Add(totalCLP, totalProtocol)
	userOutput := new(big.Int).Sub(grossOut, totalFee)
	if userOutput.Sign() < 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}

	// 7. Network 25% cut on each fee type (output units).
	networkShareNum := big.NewInt(a.cfg.NetworkShareNum)
	networkShareDen := big.NewInt(a.cfg.NetworkShareDen)
	if networkShareDen.Sign() <= 0 {
		networkShareDen = big.NewInt(4)
		networkShareNum = big.NewInt(1)
	}
	networkCLP := intmath.MulDivFloor(totalCLP, networkShareNum, networkShareDen)
	networkProtocol := intmath.MulDivFloor(totalProtocol, networkShareNum, networkShareDen)
	networkCreditOutput := new(big.Int).Add(networkCLP, networkProtocol)

	// 8. Pendulum 75% pot per fee type.
	pendulumCLP := new(big.Int).Sub(totalCLP, networkCLP)
	pendulumProtocol := new(big.Int).Sub(totalProtocol, networkProtocol)

	// 9. Split ratios from snapshot — closed form, integer math.
	fNodeBps, fNodeProtocolBps := splitFractionsBps(T, V, E, P, sBps)

	// 10. Per-pot splits (LP gets floor, node side gets residual for exact conservation).
	scale := big.NewInt(pendulum.BpsScale)
	lpCLPKept := intmath.MulDivFloor(pendulumCLP, big.NewInt(pendulum.BpsScale-fNodeBps), scale)
	nodeCLPNative := new(big.Int).Sub(pendulumCLP, lpCLPKept)

	lpProtocolKept := intmath.MulDivFloor(pendulumProtocol, big.NewInt(pendulum.BpsScale-fNodeProtocolBps), scale)
	nodeProtocolNative := new(big.Int).Sub(pendulumProtocol, lpProtocolKept)

	// 11. Single output-asset node share (CLP + Protocol portions both live in
	// the output asset under the unified model). The LP-kept portions of both
	// pots stay in the pool reserves and are surfaced as lpFeeOutput so the
	// contract can log the LP/node split explicitly; lpFeeOutput +
	// nodeShareOutput + networkCreditOutput == grossOut - userOutput.
	nodeShareOutput := new(big.Int).Add(nodeCLPNative, nodeProtocolNative)
	lpFeeOutput := new(big.Int).Add(lpCLPKept, lpProtocolKept)

	// 12. Reserve update — fees stay in pool implicitly (Y - userOutput
	// captures all retained fees). Then either drain the node share as HBD
	// directly, or do one secondary CPMM hop to convert non-HBD → HBD.
	newX := new(big.Int).Add(xReserveBig, xBig)
	newY := new(big.Int).Sub(yReserveBig, userOutput)

	nodeBucketHBD := new(big.Int)
	if assetOut == "hbd" {
		// Output is HBD: node share leaves Y directly as HBD.
		if newY.Cmp(nodeShareOutput) < 0 {
			return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
		}
		nodeBucketHBD.Set(nodeShareOutput)
		newY.Sub(newY, nodeShareOutput)
	} else {
		// Output is non-HBD (X is HBD): convert via secondary CPMM hop.
		// Closed-form: hbdOut = nodeShare · newX / (newY + nodeShare).
		denom := new(big.Int).Add(newY, nodeShareOutput)
		if denom.Sign() <= 0 {
			return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
		}
		hbdOut := intmath.MulDivFloor(nodeShareOutput, newX, denom)
		if hbdOut.Cmp(newX) > 0 {
			return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
		}
		newX.Sub(newX, hbdOut) // HBD leaves X to nodes bucket
		nodeBucketHBD.Set(hbdOut)
	}

	if newX.Sign() < 0 || newY.Sign() < 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}
	if !newX.IsInt64() || !newY.IsInt64() || !userOutput.IsInt64() || !nodeBucketHBD.IsInt64() ||
		!networkCreditOutput.IsInt64() || !lpFeeOutput.IsInt64() || !nodeShareOutput.IsInt64() {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}

	// 13. Ledger write — paired transfer from contract:<id> to pendulum:nodes
	// via the active LedgerSession. Conservation is structural: the contract
	// account is debited by the same amount the bucket is credited; nothing
	// is minted. ExecuteTransfer also enforces sufficient HBD balance on the
	// source, so an under-reserved pool fails fast rather than printing.
	if nodeBucketHBD.Sign() > 0 {
		if err := accrueNodeBucket(nodeBucketHBD.Int64()); err != nil {
			return sdkErr[wasm_context.PendulumSwapFeeResult](fmt.Errorf("%w: %s", errAccrualFailed, err.Error()))
		}
	}

	out := wasm_context.PendulumSwapFeeResult{
		UserOutput:            userOutput.Int64(),
		NewXReserve:           newX.Int64(),
		NewYReserve:           newY.Int64(),
		NetworkCreditOutput:   networkCreditOutput.Int64(),
		NodeBucketCreditedHBD: nodeBucketHBD.Int64(),
		MultiplierBps:         multiplierBps,
		SAfterBps:             sBps,
		LpShareOutput:         lpFeeOutput.Int64(),
		NodeShareOutput:       nodeShareOutput.Int64(),
	}
	return result.Ok(out)
}

func contractWhitelisted(contractID string, whitelist []string) bool {
	if contractID == "" {
		return false
	}
	for _, id := range whitelist {
		if id == contractID {
			return true
		}
	}
	return false
}

func normalizeAsset(a string) string {
	if a == "" {
		return ""
	}
	out := make([]byte, len(a))
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// tradeRatioBps returns r = x/X expressed in basis points. (0, nil) if X
// is non-positive (the stabilizer treats that as r=0 — same as a zero-size
// trade, no destabilization). On int64 overflow returns
// errArithmeticOverflow so the swap fails fast rather than feeding a
// bogus ratio into StabilizerMultiplierBps.
func tradeRatioBps(x, X int64) (int64, error) {
	if X <= 0 {
		return 0, nil
	}
	v, ok := intmath.MulDivFloorI64(x, pendulum.BpsScale, X)
	if !ok {
		return 0, errArithmeticOverflow
	}
	return v, nil
}

// exacerbatesFromSnapshot derives the stabilizer "exacerbates" hint from the
// current pendulum state and the swap direction.
//
//	s = V/E = 2P/E. HBD-in raises P (and therefore s); HBD-out lowers it.
//
// The trade exacerbates the imbalance whenever it moves s away from the
// equilibrium target s_eq (the point the StabilizerMultiplier penalizes
// deviations from). At exactly s = s_eq any nonzero swap exacerbates by
// definition.
func exacerbatesFromSnapshot(sBps int64, hbdIn bool) bool {
	target := pendulum.TargetSBps
	switch {
	case sBps == target:
		return true
	case sBps < target:
		// We want s to rise. HBD-in raises s (corrective); HBD-out lowers it.
		return !hbdIn
	default: // sBps > target
		// We want s to fall. HBD-out lowers s (corrective); HBD-in raises it.
		return hbdIn
	}
}

// splitFractionsBps returns (f_node, f_node_protocol) as basis points.
//
// Audit PEND-1 (HIGH 7.5 — CODE-PROVEN): the previous denominator mixed
// HIVE-denominated T with HBD³ terms (T·V² + P·E·(cE−V)), producing
// LP under-payment of +718 bps at devnet's price 0.0625 and +2916 bps
// at 0.30 (test t = 1 makes the bug invisible; any HIVE price ≠ 1
// exposed it). The audit's corrected form is HBD-denominated:
//
//	denom = E·((3/2)V² + P(cE − V))
//	stake_node = (3/2)·V²·E
//	f_node = stake_node / denom = (3/2)V² / ((3/2)V² + P(cE−V))
//	       = 3V² / (3V² + 2P(cE − V))      (multiply through by 2)
//
// T and E cancel out — only V, E (in cE), and P remain. T is kept on
// the function signature for caller compat but no longer participates
// in the calc.
//
// fNode
// applies to the CLP pot; fNodeProtocol applies to the protocol+surplus pot
// with §9 redirect cliffs at the safe-band edges (pendulum.RedirectLo/HiBps).
func splitFractionsBps(T, V, E, P *big.Int, sBps int64) (int64, int64) {
	_ = T // audit PEND-1: T no longer participates (cancels out under HBD-denominated stake)
	_ = E // audit PEND-1: E also cancels (the corrected form is unit-homogeneous in V and P)

	scale := big.NewInt(pendulum.BpsScale)

	cE := pendulum.CliffTimesE(E)
	if V.Cmp(cE) >= 0 {
		// Under-secured cliff: all to nodes.
		return pendulum.BpsScale, pendulum.BpsScale
	}

	// Audit PEND-1: HBD-denominated stake formula.
	//   numer = 3·V²
	//   denom = 3·V² + 2·P·(cE − V)
	//   f_node = numer · scale / denom (in bps)
	three := big.NewInt(3)
	two := big.NewInt(2)
	vSquared := new(big.Int).Mul(V, V)
	numer := new(big.Int).Mul(three, vSquared) // 3·V²
	cEMinusV := new(big.Int).Sub(cE, V)
	pTerm := new(big.Int).Mul(two, P)
	pTerm.Mul(pTerm, cEMinusV) // 2·P·(cE − V)
	denom := new(big.Int).Add(numer, pTerm)
	if denom.Sign() == 0 {
		return pendulum.BpsScale, pendulum.BpsScale
	}

	// f_node = 3V² / (3V² + 2P(cE − V)) in bps.
	fNodeBig := intmath.MulDivFloor(numer, scale, denom)
	if !fNodeBig.IsInt64() {
		return pendulum.BpsScale, pendulum.BpsScale
	}
	fNode := fNodeBig.Int64()
	if fNode > pendulum.BpsScale {
		fNode = pendulum.BpsScale
	}

	// §9: protocol leg redirects to the rebalancing side past the safe band.
	// Direction corrected vs the PDF's literal "nodes if s<low, pools if
	// s>high" (which compensated the starved side and so *dampened*
	// restoration): below RedirectLo liquidity is starved → fund LPs
	// (fNodeProtocol=0) to attract it; above RedirectHi liquidity is in excess
	// → route to nodes (fNodeProtocol=BpsScale) to shed it. Edges are the
	// safe-band edges (params.go).
	fNodeProtocol := fNode
	if sBps < pendulum.RedirectLoBps {
		fNodeProtocol = 0
	} else if sBps > pendulum.RedirectHiBps {
		fNodeProtocol = pendulum.BpsScale
	}
	return fNode, fNodeProtocol
}
