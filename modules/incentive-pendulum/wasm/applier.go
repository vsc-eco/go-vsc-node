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
	Stabilizer        pendulum.StabilizerParamsFixed
	NetworkShareNum   int64 // 1 of 4 → 25% network share
	NetworkShareDen   int64
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

// ApplySwapFees implements wasm_context.PendulumApplier.
//
// Math walkthrough is the 12-step recipe from the W3 plan section. All
// arithmetic is integer / SQ64; conversion residuals are assigned
// deterministically (LP gets the floor, node side gets the residual on the
// CLP leg; same convention on protocol leg).
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
	if args.BaseCLP < 0 {
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
	baseCLPBig := big.NewInt(args.BaseCLP)

	// 3. Protocol fee + stabilizer (input asset).
	baseProtocol := intmath.MulDivFloor(xBig, big.NewInt(int64(pendulum.ProtocolFeeRateFixed)), big.NewInt(intmath.SQ64Scale))
	rTrade, _ := intmath.SQ64Div(toSQ64Ratio(args.X, args.XReserve)) // r = x/X in SQ64
	stab := a.cfg.Stabilizer
	if !args.Exacerbates {
		// PDF §5: non-exacerbating swaps push at 0.7×.
		stab.Push = intmath.SQ64(intmath.SQ64Scale * 70 / 100)
	}
	multiplier := pendulum.StabilizerMultiplierFixed(s, rTrade, stab)
	chargedProtocol := pendulum.ApplyMultiplierFixed(baseProtocol, multiplier)
	surplus := new(big.Int).Sub(chargedProtocol, baseProtocol)
	if surplus.Sign() < 0 {
		surplus.SetInt64(0)
	}

	// 4. Total fee per side.
	totalCLP := baseCLPBig                                 // output asset
	totalProtocol := new(big.Int).Add(baseProtocol, surplus) // input asset

	// 5. Network 25% (per-side, native).
	networkShareNum := big.NewInt(a.cfg.NetworkShareNum)
	networkShareDen := big.NewInt(a.cfg.NetworkShareDen)
	if networkShareDen.Sign() <= 0 {
		networkShareDen = big.NewInt(4)
		networkShareNum = big.NewInt(1)
	}
	networkCLPNative := intmath.MulDivFloor(totalCLP, networkShareNum, networkShareDen)
	networkProtocolNative := intmath.MulDivFloor(totalProtocol, networkShareNum, networkShareDen)

	// 6. Pendulum 75% pot.
	pendulumCLP := new(big.Int).Sub(totalCLP, networkCLPNative)
	pendulumProtocol := new(big.Int).Sub(totalProtocol, networkProtocolNative)

	// 7. Split ratios from snapshot — closed form, integer math.
	fNode, fNodeProtocol := splitFractions(T, V, E, P, s)

	// 8. Per-side splits. LP gets floor, node side gets residual.
	scale := big.NewInt(intmath.SQ64Scale)
	lpCLPKept := intmath.MulDivFloor(pendulumCLP, big.NewInt(intmath.SQ64Scale-int64(fNode)), scale)
	nodeCLPNative := new(big.Int).Sub(pendulumCLP, lpCLPKept)

	lpProtocolKept := intmath.MulDivFloor(pendulumProtocol, big.NewInt(intmath.SQ64Scale-int64(fNodeProtocol)), scale)
	nodeProtocolNative := new(big.Int).Sub(pendulumProtocol, lpProtocolKept)

	// 9. Intermediate reserves before node-side conversion. The user pays the
	// full CLP fee on the output side, so user_output = gross - base_clp.
	xIntermediate := new(big.Int).Add(xReserveBig, xBig)
	xIntermediate.Add(xIntermediate, lpProtocolKept)
	xIntermediate.Add(xIntermediate, networkProtocolNative)

	grossOut := intmath.MulDivFloor(xBig, yReserveBig, new(big.Int).Add(xReserveBig, xBig))
	if grossOut.Cmp(yReserveBig) > 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}
	yIntermediate := new(big.Int).Sub(yReserveBig, grossOut)
	yIntermediate.Add(yIntermediate, lpCLPKept)
	yIntermediate.Add(yIntermediate, networkCLPNative)
	if yIntermediate.Sign() < 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}

	userOutput := new(big.Int).Sub(grossOut, baseCLPBig)
	if userOutput.Sign() < 0 {
		userOutput.SetInt64(0)
	}

	// 10. Node-side conversion to HBD via closed-form CPMM through the same pool.
	// Whichever side is non-HBD needs the secondary hop; the HBD side passes through.
	nodeBucketHBD := new(big.Int)
	if assetIn == "hbd" {
		// Input is HBD: protocol fee is in HBD (passes through), CLP fee is in
		// non-HBD output asset (needs conversion). Y is HBD (output). X is ASSET1 (input).
		// Wait — HBD→ASSET1 means X is HBD-side. Re-resolve relative to which
		// reserve holds HBD.
		nodeBucketHBD.Add(nodeBucketHBD, nodeProtocolNative) // input side is HBD
		// Output side (Y) is ASSET1 — need to swap nodeCLPNative ASSET1 → HBD.
		// But Y holds ASSET1 reserves, so we swap into the X (HBD) side.
		hbdOut := convertNonHBDLeg(nodeCLPNative, yIntermediate, xIntermediate)
		nodeBucketHBD.Add(nodeBucketHBD, hbdOut)
		// Update reserves.
		yIntermediate.Add(yIntermediate, nodeCLPNative)
		xIntermediate.Sub(xIntermediate, hbdOut)
	} else {
		// Input is ASSET1 (X), output is HBD (Y).
		// CLP fee in HBD passes through. Protocol fee in ASSET1 needs conversion.
		nodeBucketHBD.Add(nodeBucketHBD, nodeCLPNative)
		hbdOut := convertNonHBDLeg(nodeProtocolNative, xIntermediate, yIntermediate)
		nodeBucketHBD.Add(nodeBucketHBD, hbdOut)
		xIntermediate.Add(xIntermediate, nodeProtocolNative)
		yIntermediate.Sub(yIntermediate, hbdOut)
	}
	if xIntermediate.Sign() < 0 || yIntermediate.Sign() < 0 {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}

	// 11. Final reserves.
	if !xIntermediate.IsInt64() || !yIntermediate.IsInt64() || !userOutput.IsInt64() || !nodeBucketHBD.IsInt64() {
		return sdkErr[wasm_context.PendulumSwapFeeResult](errInsufficientReserves)
	}

	// 12. Ledger writes.
	if nodeBucketHBD.Sign() > 0 {
		res := a.ledger.PendulumAccrue("nodes", "hbd", nodeBucketHBD.Int64(), txID, blockHeight)
		if !res.Ok {
			return sdkErr[wasm_context.PendulumSwapFeeResult](fmt.Errorf("%w: %s", errAccrualFailed, res.Msg))
		}
	}

	out := wasm_context.PendulumSwapFeeResult{
		UserOutput:            userOutput.Int64(),
		NewXReserve:           xIntermediate.Int64(),
		NewYReserve:           yIntermediate.Int64(),
		NodeBucketCreditedHBD: nodeBucketHBD.Int64(),
		MultiplierQ8:          int64(multiplier),
		SAfterQ8:              int64(s),
		NetworkCredit: wasm_context.PendulumNetworkCredit{
			AssetInAmount:  asInt64(networkProtocolNative),
			AssetOutAmount: asInt64(networkCLPNative),
		},
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

// convertNonHBDLeg runs a closed-form CPMM swap of `amount` non-HBD tokens
// against (nonHBDReserve, hbdReserve). Returns the HBD output. If amount is
// zero or non-positive, returns 0.
func convertNonHBDLeg(amount, nonHBDReserve, hbdReserve *big.Int) *big.Int {
	if amount == nil || amount.Sign() <= 0 || nonHBDReserve.Sign() <= 0 || hbdReserve.Sign() <= 0 {
		return new(big.Int)
	}
	denom := new(big.Int).Add(nonHBDReserve, amount)
	if denom.Sign() == 0 {
		return new(big.Int)
	}
	return intmath.MulDivFloor(amount, hbdReserve, denom)
}

func asInt64(x *big.Int) int64 {
	if x == nil {
		return 0
	}
	if !x.IsInt64() {
		return 0
	}
	return x.Int64()
}
