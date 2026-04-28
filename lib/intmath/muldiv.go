// Package intmath holds shared integer / fixed-point primitives used by the
// pendulum library (and intended for reuse by ledger and settlement code).
//
// All operations assume non-negative inputs unless documented otherwise. The
// pendulum money path never uses negative values.
package intmath

import "math/big"

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
