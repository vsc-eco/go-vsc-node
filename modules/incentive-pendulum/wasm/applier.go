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
	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
	pendulum "vsc-node/modules/incentive-pendulum"
	wasm_context "vsc-node/modules/wasm/context"

	"github.com/JustinKnueppel/go-result"
)

// SnapshotReader is the read-side of pendulum_oracle.PendulumOracleSnapshots
// the applier needs. Pulled out so tests can stub it without standing up
// MongoDB.
type SnapshotReader interface {
	GetSnapshotAtOrBefore(blockHeight uint64) (*pendulum_oracle.SnapshotRecord, bool, error)
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
	snapshots SnapshotReader
	whitelist WhitelistGetter
	cfg       Config
}

// New constructs an Applier. Any nil dep produces an applier that always
// returns ErrUnimplemented from ApplySwapFees, so the wasm runtime can call
// without worrying about partial wiring.
func New(snapshots SnapshotReader, whitelist WhitelistGetter, cfg Config) *Applier {
	return &Applier{snapshots: snapshots, whitelist: whitelist, cfg: cfg}
}

// Sentinel errors. Wrapped with contracts.SDK_ERROR so the wasm runtime maps
// them to the standard sdk-error code.
var (
	errNotWhitelisted       = errors.New("contract not whitelisted")
	errSnapshotUnavailable  = errors.New("pendulum snapshot unavailable")
	errInvalidArgument      = errors.New("invalid pendulum swap argument")
	errInsufficientReserves = errors.New("insufficient reserves for pendulum split")
	errAccrualFailed        = errors.New("pendulum accrual failed")
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
	if a == nil || a.snapshots == nil || a.whitelist == nil || accrueNodeBucket == nil {
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

	// 2. Read snapshot.
	snap, ok, err := a.snapshots.GetSnapshotAtOrBefore(blockHeight)
	if err != nil || !ok || snap == nil || !snap.GeometryOK {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errSnapshotUnavailable)
	}
	V := big.NewInt(snap.GeometryV)
	E := big.NewInt(snap.GeometryE)
	P := big.NewInt(snap.GeometryP)
	T := big.NewInt(snap.GeometryT)
	sBps := snap.GeometrySBps
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
	// corrective when its direction matches the move toward s = 0.5; any
	// move away (including any move at exactly s = 0.5) is exacerbating.
	rTradeBps := tradeRatioBps(args.X, args.XReserve)
	stab := a.cfg.Stabilizer
	if !exacerbatesFromSnapshot(sBps, assetIn == "hbd") {
		// PDF §5: non-exacerbating swaps push at 0.7×.
		stab.PushBps = pendulum.BpsScale * 70 / 100
	}
	multiplierBps := pendulum.StabilizerMultiplierBps(sBps, rTradeBps, stab)
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
	// the output asset under the unified model).
	nodeShareOutput := new(big.Int).Add(nodeCLPNative, nodeProtocolNative)

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
		!networkCreditOutput.IsInt64() {
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

// tradeRatioBps returns r = x/X expressed in basis points. 0 if X is non-positive.
func tradeRatioBps(x, X int64) int64 {
	if X <= 0 {
		return 0
	}
	return intmath.MulDivFloorI64(x, pendulum.BpsScale, X)
}

// exacerbatesFromSnapshot derives the stabilizer "exacerbates" hint from the
// current pendulum state and the swap direction.
//
//	s = V/E = 2P/E. HBD-in raises P (and therefore s); HBD-out lowers it.
//
// The trade exacerbates the imbalance whenever it moves s away from 0.5
// (the equilibrium target the StabilizerMultiplier penalizes deviations
// from). At exactly s = 0.5 any nonzero swap exacerbates by definition.
func exacerbatesFromSnapshot(sBps int64, hbdIn bool) bool {
	const half = pendulum.BpsScale / 2
	switch {
	case sBps == half:
		return true
	case sBps < half:
		// We want s to rise. HBD-in raises s (corrective); HBD-out lowers it.
		return !hbdIn
	default: // sBps > half
		// We want s to fall. HBD-out lowers s (corrective); HBD-in raises it.
		return hbdIn
	}
}

// splitFractionsBps returns (f_node, f_node_protocol) as basis points. fNode
// applies to the CLP pot; fNodeProtocol applies to the protocol+surplus pot
// with PDF §9 redirect cliffs at s ∈ {0.3, 0.7}.
func splitFractionsBps(T, V, E, P *big.Int, sBps int64) (int64, int64) {
	scale := big.NewInt(pendulum.BpsScale)

	if V.Cmp(E) >= 0 {
		// Cliff: all to nodes.
		return pendulum.BpsScale, pendulum.BpsScale
	}

	// denom = T·V² + P·E·(E − V)
	tvSquared := new(big.Int).Mul(V, V)
	tvSquared.Mul(tvSquared, T)
	eMinusV := new(big.Int).Sub(E, V)
	peTerm := new(big.Int).Mul(P, E)
	peTerm.Mul(peTerm, eMinusV)
	denom := new(big.Int).Add(tvSquared, peTerm)
	if denom.Sign() == 0 {
		return pendulum.BpsScale, pendulum.BpsScale
	}

	// f_node = T·V² / denom in bps.
	fNodeBig := intmath.MulDivFloor(tvSquared, scale, denom)
	if !fNodeBig.IsInt64() {
		return pendulum.BpsScale, pendulum.BpsScale
	}
	fNode := fNodeBig.Int64()
	if fNode > pendulum.BpsScale {
		fNode = pendulum.BpsScale
	}

	// PDF §9: protocol leg redirects past extreme s.
	const sLowBps = pendulum.BpsScale * 30 / 100  // 0.3
	const sHighBps = pendulum.BpsScale * 70 / 100 // 0.7
	fNodeProtocol := fNode
	if sBps < sLowBps {
		fNodeProtocol = pendulum.BpsScale
	} else if sBps > sHighBps {
		fNodeProtocol = 0
	}
	return fNode, fNodeProtocol
}
