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
	ledgerSystem "vsc-node/modules/ledger-system"
	wasm_context "vsc-node/modules/wasm/context"

	"github.com/JustinKnueppel/go-result"
)

// SnapshotReader is the read-side of pendulum_oracle.PendulumOracleSnapshots
// the applier needs. Pulled out so tests can stub it without standing up
// MongoDB.
type SnapshotReader interface {
	GetSnapshotAtOrBefore(blockHeight uint64) (*pendulum_oracle.SnapshotRecord, bool, error)
}

// LedgerAccruer is the narrow PendulumAccrue side of the ledger system the
// applier touches. Same testability rationale as SnapshotReader.
type LedgerAccruer interface {
	PendulumAccrue(account, asset string, amount int64, txID string, blockHeight uint64) ledgerSystem.LedgerResult
}

// WhitelistGetter returns the current pool whitelist (returned slice may be a
// fresh copy; the applier reads it through this hook so live config changes
// are picked up without re-construction).
type WhitelistGetter func() []string

// Config is the per-network tuning the applier reads on every swap.
type Config struct {
	Stabilizer      pendulum.StabilizerParamsFixed
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
		Stabilizer:      pendulum.DefaultStabilizerParamsFixed(),
		NetworkShareNum: 1,
		NetworkShareDen: 4,
	}
}

// Applier is the concrete PendulumApplier impl wired by the state engine.
type Applier struct {
	snapshots SnapshotReader
	ledger    LedgerAccruer
	whitelist WhitelistGetter
	cfg       Config
}

// New constructs an Applier. Any nil dep produces an applier that always
// returns ErrUnimplemented from ApplySwapFees, so the wasm runtime can call
// without worrying about partial wiring.
func New(snapshots SnapshotReader, ledger LedgerAccruer, whitelist WhitelistGetter, cfg Config) *Applier {
	return &Applier{snapshots: snapshots, ledger: ledger, whitelist: whitelist, cfg: cfg}
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
//   newX = X + x                                (entire input enters the pool)
//   newY = Y - userOutput                       (the user takes only userOutput)
//                                                — all fees stay in pool reserves
//   then if assetOut == "hbd":
//     newY -= nodeShareOutput                   (HBD leaves pool to nodes bucket)
//   else (assetOut is non-HBD):
//     hbdOut = nodeShareOutput · newX / (newY + nodeShareOutput)
//     newY += nodeShareOutput                   (virtual: non-HBD added back)
//     newX -= hbdOut                            (HBD leaves X reserve to nodes bucket)
func (a *Applier) ApplySwapFees(contractID, txID string, blockHeight uint64, args wasm_context.PendulumSwapFeeArgs) result.Result[wasm_context.PendulumSwapFeeResult] {
	if a == nil || a.snapshots == nil || a.ledger == nil || a.whitelist == nil {
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
	s := intmath.SQ64(snap.GeometryS)
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
	baseProtocol := intmath.MulDivFloor(grossOut, big.NewInt(int64(pendulum.ProtocolFeeRateFixed)), big.NewInt(intmath.SQ64Scale))

	// 4. Stabilizer multiplier on the protocol fee. PDF §5: the stabilizer
	// inflates the protocol fee specifically; the surplus is what funds the
	// rebalancing incentive. CLP fee is unaffected by m.
	//
	// "Exacerbates" is derived from snapshot s and the swap direction — the
	// SDK has all inputs, so accepting a contract-supplied hint would be a
	// pure non-determinism vector with no information gain. HBD-in raises
	// P (and therefore s = V/E = 2P/E); HBD-out lowers it. A swap is
	// corrective when its direction matches the move toward s = 0.5; any
	// move away (including any move at exactly s = 0.5) is exacerbating.
	rTrade, _ := intmath.SQ64Div(toSQ64Ratio(args.X, args.XReserve)) // r = x/X in SQ64
	stab := a.cfg.Stabilizer
	if !exacerbatesFromSnapshot(s, assetIn == "hbd") {
		// PDF §5: non-exacerbating swaps push at 0.7×.
		stab.Push = intmath.SQ64(intmath.SQ64Scale * 70 / 100)
	}
	multiplier := pendulum.StabilizerMultiplierFixed(s, rTrade, stab)
	chargedProtocol := pendulum.ApplyMultiplierFixed(baseProtocol, multiplier)
	surplus := new(big.Int).Sub(chargedProtocol, baseProtocol)
	if surplus.Sign() < 0 {
		surplus.SetInt64(0)
	}

	// 5. Total fees per type, all output units.
	totalCLP := baseCLP
	totalProtocol := new(big.Int).Add(baseProtocol, surplus)

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
	fNode, fNodeProtocol := splitFractions(T, V, E, P, s)

	// 10. Per-pot splits (LP gets floor, node side gets residual for exact conservation).
	scale := big.NewInt(intmath.SQ64Scale)
	lpCLPKept := intmath.MulDivFloor(pendulumCLP, big.NewInt(intmath.SQ64Scale-int64(fNode)), scale)
	nodeCLPNative := new(big.Int).Sub(pendulumCLP, lpCLPKept)

	lpProtocolKept := intmath.MulDivFloor(pendulumProtocol, big.NewInt(intmath.SQ64Scale-int64(fNodeProtocol)), scale)
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
		newY.Add(newY, nodeShareOutput) // virtual: non-HBD added back to Y
		newX.Sub(newX, hbdOut)          // HBD leaves X to nodes bucket
		nodeBucketHBD.Set(hbdOut)
	}

	if newX.Sign() < 0 || newY.Sign() < 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}
	if !newX.IsInt64() || !newY.IsInt64() || !userOutput.IsInt64() || !nodeBucketHBD.IsInt64() || !networkCreditOutput.IsInt64() {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}

	// 13. Ledger write.
	if nodeBucketHBD.Sign() > 0 {
		res := a.ledger.PendulumAccrue("nodes", "hbd", nodeBucketHBD.Int64(), txID, blockHeight)
		if !res.Ok {
			return sdkErr[wasm_context.PendulumSwapFeeResult](fmt.Errorf("%w: %s", errAccrualFailed, res.Msg))
		}
	}

	out := wasm_context.PendulumSwapFeeResult{
		UserOutput:            userOutput.Int64(),
		NewXReserve:           newX.Int64(),
		NewYReserve:           newY.Int64(),
		NetworkCreditOutput:   networkCreditOutput.Int64(),
		NodeBucketCreditedHBD: nodeBucketHBD.Int64(),
		MultiplierQ8:          int64(multiplier),
		SAfterQ8:              int64(s),
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

// toSQ64Ratio converts a/b to SQ64 inputs for SQ64Div: returns (a*SQ64Scale, b).
// Rolled out separately so the caller stays readable.
func toSQ64Ratio(a, b int64) (intmath.SQ64, intmath.SQ64) {
	if b == 0 {
		return 0, 1
	}
	return intmath.SQ64(a), intmath.SQ64(b)
}

// exacerbatesFromSnapshot derives the stabilizer "exacerbates" hint from the
// current pendulum state and the swap direction.
//
//   s = V/E = 2P/E. HBD-in raises P (and therefore s); HBD-out lowers it.
//
// The trade exacerbates the imbalance whenever it moves s away from 0.5
// (the equilibrium target the StabilizerMultiplier penalizes deviations
// from). At exactly s = 0.5 any nonzero swap exacerbates by definition.
func exacerbatesFromSnapshot(s intmath.SQ64, hbdIn bool) bool {
	const half = intmath.SQ64(intmath.SQ64Scale / 2)
	switch {
	case s == half:
		return true
	case s < half:
		// We want s to rise. HBD-in raises s (corrective); HBD-out lowers it.
		return !hbdIn
	default: // s > half
		// We want s to fall. HBD-out lowers s (corrective); HBD-in raises it.
		return hbdIn
	}
}

// splitFractions returns (f_node, f_node_protocol) as SQ64. fNode applies to
// the CLP pot; fNodeProtocol applies to the protocol+surplus pot with PDF §9
// redirect cliffs at s ∈ {0.3, 0.7}.
func splitFractions(T, V, E, P *big.Int, s intmath.SQ64) (intmath.SQ64, intmath.SQ64) {
	scale := big.NewInt(intmath.SQ64Scale)

	if V.Cmp(E) >= 0 {
		// Cliff: all to nodes.
		return intmath.SQ64(intmath.SQ64Scale), intmath.SQ64(intmath.SQ64Scale)
	}

	// denom = T·V² + P·E·(E − V)
	tvSquared := new(big.Int).Mul(V, V)
	tvSquared.Mul(tvSquared, T)
	eMinusV := new(big.Int).Sub(E, V)
	peTerm := new(big.Int).Mul(P, E)
	peTerm.Mul(peTerm, eMinusV)
	denom := new(big.Int).Add(tvSquared, peTerm)
	if denom.Sign() == 0 {
		return intmath.SQ64(intmath.SQ64Scale), intmath.SQ64(intmath.SQ64Scale)
	}

	// f_node = T·V² / denom in SQ64
	fNodeBig := intmath.MulDivFloor(tvSquared, scale, denom)
	if !fNodeBig.IsInt64() {
		return intmath.SQ64(intmath.SQ64Scale), intmath.SQ64(intmath.SQ64Scale)
	}
	fNode := intmath.SQ64(fNodeBig.Int64())
	if fNode > intmath.SQ64(intmath.SQ64Scale) {
		fNode = intmath.SQ64(intmath.SQ64Scale)
	}

	// PDF §9: protocol leg redirects past extreme s.
	const sLow = intmath.SQ64(intmath.SQ64Scale * 30 / 100)  // 0.3
	const sHigh = intmath.SQ64(intmath.SQ64Scale * 70 / 100) // 0.7
	fNodeProtocol := fNode
	if s < sLow {
		fNodeProtocol = intmath.SQ64(intmath.SQ64Scale)
	} else if s > sHigh {
		fNodeProtocol = 0
	}
	return fNode, fNodeProtocol
}
