package rc_system

import (
	"math/big"

	"vsc-node/modules/common/params"
)

// CalculateFrozenBal returns how much of initialBal is still frozen at `end`,
// given it started freezing at `start` and linearly returns over
// RC_RETURN_PERIOD.
//
// review2 MEDIUM #107: this was
//
//	int64(diff * uint64(initialBal) / params.RC_RETURN_PERIOD)
//
// where `uint64(initialBal)` wraps to ~1.8e19 for a negative balance, and
// `diff * uint64(...)` overflows uint64 for large values — both yield a
// wrong frozen-RC amount, which feeds tx admission. It is deterministic
// (same on every node) but still incorrect. A `start > end` underflow made
// `diff` huge → clamped to "fully returned" (frozen 0), under-charging an
// account on a node that is briefly behind.
//
// Behaviour is identical to the old code for the valid domain
// (0 <= initialBal, start <= end, diff < RC_RETURN_PERIOD, no overflow);
// only the degenerate/overflow cases are now well-defined.
func CalculateFrozenBal(start, end uint64, initialBal int64) int64 {
	if initialBal <= 0 {
		return 0 // nothing (and never a negative amount) frozen
	}
	if end <= start {
		return initialBal // no time elapsed (or clock went backwards) → fully frozen
	}
	period := params.RC_RETURN_PERIOD
	diff := end - start
	if period == 0 || diff >= period {
		return 0 // fully returned
	}
	// amtRet = initialBal * diff / period, computed without overflow.
	// Since diff < period, amtRet < initialBal <= MaxInt64, so Int64() is exact.
	amt := new(big.Int).Mul(big.NewInt(initialBal), new(big.Int).SetUint64(diff))
	amt.Div(amt, new(big.Int).SetUint64(period))
	amtRet := amt.Int64()
	if amtRet > initialBal {
		amtRet = initialBal
	}
	if amtRet < 0 {
		amtRet = 0
	}
	return initialBal - amtRet
}
