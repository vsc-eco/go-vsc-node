package pendulumwasm

import (
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"strings"

	"vsc-node/modules/common/consensusversion"
	pendulum "vsc-node/modules/incentive-pendulum"
	wasm_context "vsc-node/modules/wasm/context"
)

// StableListing is the node's own record of a pool that prices on the
// StableSwap curve (system-config StablePool): the amplification and the
// pair with each asset's decimals.
type StableListing struct {
	Amp       int64
	Asset0    string
	Decimals0 uint8
	Asset1    string
	Decimals1 uint8
}

// StablePoolLookup returns the StableSwap listing of a pool contract in force
// at a height (system-config StablePoolAt).
type StablePoolLookup func(contractID string, blockHeight uint64) (StableListing, bool)

var (
	errStableUnlisted = errors.New("stable curve requested by a pool the network does not list as stable")
	errStableNotAsked = errors.New("pool is listed as stable but did not ask for the stable curve")
	errStableMismatch = errors.New("stable pool parameters differ from the network's listing")
	errStableCurve    = errors.New("unknown curve")
)

// stableTerms are a stable swap's parameters once checked against the listing.
type stableTerms struct {
	amp  int64
	rIn  *big.Int // 10^(18 − decimals of the input asset)
	rOut *big.Int
}

// resolveStable decides how a swap is priced. nil terms and nil error = the
// constant product, exactly as before 0.13.0.
//
// Below consensus 0.13.0 nothing here is read: every pool prices on the
// constant product whatever it sends, as a binary without this code does.
// From 0.13.0 the curve is stable iff the pool is listed at this height, the
// contract asks for "stable", its amplification and decimals equal the
// listing, and the pair is the listed pair. A listed pool that does not ask,
// or a pool that asks without a listing, or any mismatch, is refused: the
// stable pool contract accepts only the stable curve, and a constant-product
// contract would reject stable outputs.
func (a *Applier) resolveStable(
	contractID string,
	blockHeight uint64,
	args wasm_context.PendulumSwapFeeArgs,
	assetIn, assetOut string,
) (*stableTerms, error) {
	if a.consensusAt == nil || !consensusversion.StableSwapActive(a.consensusAt(blockHeight)) {
		return nil, nil
	}
	curve, ok := rawString(args.Curve)
	if !ok {
		return nil, errStableCurve
	}
	var listing StableListing
	listed := false
	if a.cfg.StablePoolAt != nil {
		listing, listed = a.cfg.StablePoolAt(contractID, blockHeight)
	}
	switch curve {
	case "":
		if listed {
			return nil, errStableNotAsked
		}
		return nil, nil
	case "stable":
		if !listed {
			return nil, errStableUnlisted
		}
	default:
		return nil, errStableCurve
	}

	amp, okAmp := rawInt(args.Amp)
	decIn, okIn := rawInt(args.DecIn)
	decOut, okOut := rawInt(args.DecOut)
	if !okAmp || !okIn || !okOut || amp != listing.Amp {
		return nil, errStableMismatch
	}
	var wantIn, wantOut uint8
	switch {
	case assetIn == listing.Asset0 && assetOut == listing.Asset1:
		wantIn, wantOut = listing.Decimals0, listing.Decimals1
	case assetIn == listing.Asset1 && assetOut == listing.Asset0:
		wantIn, wantOut = listing.Decimals1, listing.Decimals0
	default:
		return nil, errStableMismatch
	}
	if decIn != int64(wantIn) || decOut != int64(wantOut) {
		return nil, errStableMismatch
	}
	rIn, err := pendulum.StableRate(wantIn)
	if err != nil {
		return nil, errStableMismatch
	}
	rOut, err := pendulum.StableRate(wantOut)
	if err != nil {
		return nil, errStableMismatch
	}
	return &stableTerms{amp: amp, rIn: rIn, rOut: rOut}, nil
}

// rawString reads a raw JSON value that must be a string; absent = "".
func rawString(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return "", false
	}
	return s, true
}

// rawInt reads a raw JSON value that is an integer or a decimal string.
func rawInt(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return 0, false
		}
		raw = s
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// stableGross prices x (input base units) against reserves X (input) and Y
// (output) on the curve: the gross output in output base units, floored and
// less one unit (Curve's rounding margin, in the pool's favour), and the slip
// fee on that output measured against the curve's spot price.
//
// Why the pool contract's floor (stored reserves on or above the pre-swap
// level set, pendulum.StableFloorY) always holds for these outputs: every
// output here and in stableSellBack keeps at least one BASE unit in the pool,
// which is 10^12..10^15 normalized units, against a Newton error of at most
// 2 normalized units in StableGetD/StableGetY. Fees only add to that. Checked
// over random balanced and 30:1 pools by
// TestStable_EverySwapPassesTheContractFloor.
func stableGross(t *stableTerms, x, X, Y *big.Int) (grossOut, baseCLP *big.Int, err error) {
	xp := new(big.Int).Mul(X, t.rIn)
	yp := new(big.Int).Mul(Y, t.rOut)
	dx := new(big.Int).Mul(x, t.rIn)
	d0, err := pendulum.StableGetD(xp, yp, t.amp)
	if err != nil {
		return nil, nil, err
	}
	yNew, err := pendulum.StableGetY(new(big.Int).Add(xp, dx), d0, t.amp)
	if err != nil {
		return nil, nil, err
	}
	dy := new(big.Int).Sub(yp, yNew)
	if dy.Sign() <= 0 {
		return nil, nil, errInsufficientReserves
	}
	grossOut = dy.Quo(dy, t.rOut)
	grossOut.Sub(grossOut, big.NewInt(1))
	if grossOut.Sign() <= 0 || grossOut.Cmp(Y) >= 0 {
		return nil, nil, errInsufficientReserves
	}
	pNum, pDen, err := pendulum.StableMarginalPrice(xp, yp, d0, t.amp)
	if err != nil {
		return nil, nil, err
	}
	// Spot value of the input in OUTPUT BASE units: dx·pNum/(pDen·rOut).
	baseCLP = pendulum.SlipFee(grossOut, dx, pNum, new(big.Int).Mul(pDen, t.rOut))
	return grossOut, baseCLP, nil
}

// stableSellBack sells the node share (output asset, not HBD) back to the
// pool for HBD on the same curve, from the pool's LP reserve after the main
// swap (xAfter input/HBD, yLP output). Returns the HBD paid out, floored and
// less one unit. The constant-product hop would price it at the reserve
// ratio, which on an imbalanced stable pool is far from the price.
func stableSellBack(t *stableTerms, share, xAfter, yLP *big.Int) (*big.Int, error) {
	if share.Sign() <= 0 {
		return new(big.Int), nil
	}
	if yLP.Sign() <= 0 || xAfter.Sign() <= 0 {
		return nil, errInsufficientReserves
	}
	xp := new(big.Int).Mul(xAfter, t.rIn)
	yp := new(big.Int).Mul(yLP, t.rOut)
	d1, err := pendulum.StableGetD(xp, yp, t.amp)
	if err != nil {
		return nil, err
	}
	xNew, err := pendulum.StableGetY(new(big.Int).Add(yp, new(big.Int).Mul(share, t.rOut)), d1, t.amp)
	if err != nil {
		return nil, err
	}
	out := new(big.Int).Sub(xp, xNew)
	if out.Sign() <= 0 {
		return new(big.Int), nil
	}
	out.Quo(out, t.rIn)
	out.Sub(out, big.NewInt(1))
	if out.Sign() < 0 {
		return new(big.Int), nil
	}
	return out, nil
}
