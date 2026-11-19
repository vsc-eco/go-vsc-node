package pendulum

import (
	"errors"
	"math/big"
)

// StableSwap, two assets, in the integer form of Curve's first pools
// (StableSwap invariant with Ann = A·n, n = 2):
//
//	Ann·(x + y) + D = Ann·D + D³ / (4·x·y)
//
// Used by the one pool the network lists as stable (system-config
// StablePools); every other pool keeps the constant product. Reserves are
// NORMALIZED before they reach these functions: each asset's base units are
// multiplied by 10^(18 − decimals) (Curve's rate multipliers), so both sides
// count in the same 18-decimal unit (HBD has 3 decimals, USDC 6) and the
// flooring inside the Newton steps stays far below one base unit.
//
// Consensus code: big.Int only, every division floors, every loop is capped.
// The pool contract (dex-contracts) carries a byte-for-byte copy of
// StableGetD and StableGetY and checks every swap against them, so the two
// must not drift.

// StableMaxIterations caps both Newton loops. Curve uses the same cap; the
// loops converge in a handful of steps for any sane reserves.
const StableMaxIterations = 255

// StableMaxAmp bounds the amplification coefficient A.
const StableMaxAmp int64 = 10_000

var (
	// ErrStableInput: a non-positive reserve or an amplification outside
	// [1, StableMaxAmp].
	ErrStableInput = errors.New("pendulum: stableswap invalid input")
	// ErrStableNoConverge: a Newton loop did not settle within
	// StableMaxIterations. The swap is refused rather than priced on an
	// unconverged value.
	ErrStableNoConverge = errors.New("pendulum: stableswap did not converge")
)

var (
	bigOne   = big.NewInt(1)
	bigTwo   = big.NewInt(2)
	bigThree = big.NewInt(3)
	bigFour  = big.NewInt(4)
)

func stableAnn(amp int64) (*big.Int, error) {
	if amp < 1 || amp > StableMaxAmp {
		return nil, ErrStableInput
	}
	return big.NewInt(amp * 2), nil
}

// StableGetD returns the invariant D for normalized reserves (x, y).
//
//	D_P    = D³ / (4·x·y)                       (one floor division)
//	D_next = (Ann·S + 2·D_P)·D / ((Ann − 1)·D + 3·D_P)
//
// starting from D = S = x + y, until |D_next − D| ≤ 1.
func StableGetD(x, y *big.Int, amp int64) (*big.Int, error) {
	if x == nil || y == nil || x.Sign() <= 0 || y.Sign() <= 0 {
		return nil, ErrStableInput
	}
	ann, err := stableAnn(amp)
	if err != nil {
		return nil, err
	}
	s := new(big.Int).Add(x, y)
	d := new(big.Int).Set(s)
	annS := new(big.Int).Mul(ann, s)
	annMinusOne := new(big.Int).Sub(ann, bigOne)
	fourXY := new(big.Int).Mul(x, y)
	fourXY.Mul(fourXY, bigFour)
	for i := 0; i < StableMaxIterations; i++ {
		// One floor division. Curve splits it (D·D/(2x)·D/(2y)) only to fit
		// uint256; the double flooring loses enough at a 1e5:1 imbalance for
		// the integer iteration to fall into a 2-cycle that never settles.
		dp := new(big.Int).Mul(d, d)
		dp.Mul(dp, d)
		dp.Quo(dp, fourXY)

		prev := new(big.Int).Set(d)
		num := new(big.Int).Mul(dp, bigTwo)
		num.Add(num, annS)
		num.Mul(num, d)
		den := new(big.Int).Mul(annMinusOne, d)
		den.Add(den, new(big.Int).Mul(bigThree, dp))
		if den.Sign() <= 0 {
			return nil, ErrStableNoConverge
		}
		d = num.Quo(num, den)
		if diff := new(big.Int).Sub(d, prev); diff.CmpAbs(bigOne) <= 0 {
			return d, nil
		}
	}
	return nil, ErrStableNoConverge
}

// StableGetY returns the other reserve that keeps invariant D when one
// reserve becomes xNew (the invariant is symmetric, so this serves either
// side). Solves y² + (b − D)·y = c with
//
//	c = D³ / (4·xNew·Ann)   (one floor division)
//	b = xNew + D/Ann
//
// by y_next = (y² + c) / (2y + b − D) from y = D, until |y_next − y| ≤ 1.
func StableGetY(xNew, d *big.Int, amp int64) (*big.Int, error) {
	if xNew == nil || d == nil || xNew.Sign() <= 0 || d.Sign() <= 0 {
		return nil, ErrStableInput
	}
	ann, err := stableAnn(amp)
	if err != nil {
		return nil, err
	}
	c := new(big.Int).Mul(d, d)
	c.Mul(c, d)
	c.Quo(c, new(big.Int).Mul(new(big.Int).Mul(xNew, ann), bigFour))
	b := new(big.Int).Quo(d, ann)
	b.Add(b, xNew)

	y := new(big.Int).Set(d)
	for i := 0; i < StableMaxIterations; i++ {
		prev := new(big.Int).Set(y)
		num := new(big.Int).Mul(y, y)
		num.Add(num, c)
		den := new(big.Int).Mul(y, bigTwo)
		den.Add(den, b)
		den.Sub(den, d)
		if den.Sign() <= 0 {
			return nil, ErrStableNoConverge
		}
		y = num.Quo(num, den)
		if diff := new(big.Int).Sub(y, prev); diff.CmpAbs(bigOne) <= 0 {
			return y, nil
		}
	}
	return nil, ErrStableNoConverge
}

// StableMarginalPrice returns the spot price of one normalized unit of x in
// normalized units of y, as the fraction num/den. From the partial
// derivatives of the invariant:
//
//	p = (4·Ann·x²·y² + D³·y) / (4·Ann·x²·y² + D³·x)
//
// (→ y/x as A → 0, the constant-product price; → 1 as A → ∞.)
func StableMarginalPrice(x, y, d *big.Int, amp int64) (num, den *big.Int, err error) {
	if x == nil || y == nil || d == nil || x.Sign() <= 0 || y.Sign() <= 0 || d.Sign() <= 0 {
		return nil, nil, ErrStableInput
	}
	ann, err := stableAnn(amp)
	if err != nil {
		return nil, nil, err
	}
	xy := new(big.Int).Mul(x, y)
	core := new(big.Int).Mul(xy, xy)
	core.Mul(core, ann)
	core.Mul(core, bigFour)
	d3 := new(big.Int).Mul(d, d)
	d3.Mul(d3, d)
	num = new(big.Int).Add(core, new(big.Int).Mul(d3, y))
	den = new(big.Int).Add(core, new(big.Int).Mul(d3, x))
	return num, den, nil
}

// SlipFee returns a swap's slip fee in output units: grossOut times the
// swap's slip, where slip = 1 − grossOut / (x·p0) and p0 = pNum/pDen is the
// spot price of one input unit in output units before the swap:
//
//	fee = grossOut − grossOut² · pDen / (x · pNum)      clamped to [0, grossOut]
//
// THORChain's CLP fee x²·Y/(x+X)² is exactly this rule on a constant-product
// pool (p0 = Y/X, grossOut = x·Y/(x+X)); here it is applied to the curve the
// pool actually prices on.
func SlipFee(grossOut, x, pNum, pDen *big.Int) *big.Int {
	if grossOut == nil || x == nil || pNum == nil || pDen == nil ||
		grossOut.Sign() <= 0 || x.Sign() <= 0 || pNum.Sign() <= 0 || pDen.Sign() <= 0 {
		return new(big.Int)
	}
	sub := new(big.Int).Mul(grossOut, grossOut)
	sub.Mul(sub, pDen)
	sub.Quo(sub, new(big.Int).Mul(x, pNum))
	fee := new(big.Int).Sub(grossOut, sub)
	if fee.Sign() < 0 {
		return new(big.Int)
	}
	return fee
}

// StablePrecisionDecimals is the normalized unit: 18 decimals, as Curve.
const StablePrecisionDecimals = 18

// StableRate returns 10^(18 − decimals), the multiplier that turns an asset's
// base units into the pool's normalized unit. Decimals above 18 are refused.
func StableRate(decimals uint8) (*big.Int, error) {
	if decimals > StablePrecisionDecimals {
		return nil, ErrStableInput
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(StablePrecisionDecimals-decimals)), nil), nil
}

// StableFloorY is the pool contract's swap check: the least output-side
// reserve (normalized) that keeps the invariant of the pre-swap reserves
// (X, Y) when the input-side reserve is storedX. A swap is honest iff
// storedY·rateY ≥ StableFloorY(...). All arguments in base units except the
// result.
func StableFloorY(storedX, X, Y, rateX, rateY *big.Int, amp int64) (*big.Int, error) {
	if storedX == nil || X == nil || Y == nil || rateX == nil || rateY == nil ||
		rateX.Sign() <= 0 || rateY.Sign() <= 0 {
		return nil, ErrStableInput
	}
	d0, err := StableGetD(new(big.Int).Mul(X, rateX), new(big.Int).Mul(Y, rateY), amp)
	if err != nil {
		return nil, err
	}
	return StableGetY(new(big.Int).Mul(storedX, rateX), d0, amp)
}
