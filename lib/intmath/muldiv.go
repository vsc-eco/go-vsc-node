// Package intmath holds shared integer / fixed-point primitives used by the
// pendulum library (and intended for reuse by ledger and settlement code).
//
// All operations assume non-negative inputs unless documented otherwise. The
// pendulum money path never uses negative values.
package intmath

import "math/big"

// BpsScale is the basis-point denominator. 1.0 == 10_000 bps; 0.01 == 100 bps.
// Pendulum, fee, and ratio math use this scale uniformly so a single integer
// representation covers stabilizer params, the V/E ratio, the per-pot node
// fractions, and any HBD-per-HIVE price.
const BpsScale int64 = 10_000

// MulDivFloor returns floor(a*b/c) without mutating its inputs. Panics on c == 0.
//
// Used to compute terms like R · X / Y where R, X, Y are integer base units
// and R · X may overflow int64 even though the final quotient fits.
func MulDivFloor(a, b, c *big.Int) *big.Int {
	if c.Sign() == 0 {
		panic("intmath.MulDivFloor: divide by zero")
	}
	out := new(big.Int).Mul(a, b)
	out.Quo(out, c) // Quo truncates toward zero; for non-negative operands this is floor.
	return out
}

// MulDivFloorI64 is MulDivFloor for callers that already have int64 values.
// Promotes through big.Int to avoid intermediate overflow on a*b, then
// returns (val, true) on success or (0, false) if the floor doesn't fit
// int64. Panics on c == 0.
//
// Overflow is a hard failure callers must handle — the previous saturate-
// to-zero behavior was load-bearing for the pendulum stabilizer's cap
// clamp: a tail of 0 silently produced m = BpsScale (multiplier 1.0,
// stabilizer disabled) instead of tripping the cap. Returning ok=false
// forces the caller to abort the operation.
func MulDivFloorI64(a, b, c int64) (int64, bool) {
	if c == 0 {
		panic("intmath.MulDivFloorI64: divide by zero")
	}
	out := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	out.Quo(out, big.NewInt(c))
	if !out.IsInt64() {
		return 0, false
	}
	return out.Int64(), true
}
