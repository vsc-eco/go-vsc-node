package ledgerSystem

import "math/big"

// computeEndingAvg returns the TWAB (time-weighted average balance) used by
// the HBD interest distribution:
//
//	endingAvg = (HBD_AVG + HBD_SAVINGS * A) / B
//
// where A = blocks since last balance modification, B = total blocks since
// last claim.
//
// review4 HIGH #15: prior int64 form silently overflowed when
// HBD_SAVINGS * A exceeded ~9.2e18. With realistic mainnet balances (1e16)
// and long inter-claim intervals (~1e3 blocks), the int64 wrap is
// reachable and corrupts the entire epoch's interest. Promoted through
// big.Int with a fail-closed (ok=false) return when the final TWAB
// doesn't fit int64.
func computeEndingAvg(hbdAvg int64, hbdSavings int64, a int64, b int64) (int64, bool) {
	if b == 0 {
		return 0, false
	}
	out := new(big.Int).Mul(big.NewInt(hbdSavings), big.NewInt(a))
	out.Add(out, big.NewInt(hbdAvg))
	out.Quo(out, big.NewInt(b))
	if !out.IsInt64() {
		return 0, false
	}
	return out.Int64(), true
}

// computeDistributeAmount returns balance.HBD_AVG * amount / totalAvg using
// big.Int internally so the intermediate product doesn't wrap when both
// HBD_AVG and amount are large. Final ratio is bounded by `amount` so
// always fits int64, but the call returns ok=false on overflow as a
// belt-and-braces guard.
//
// review4 HIGH #15 (companion).
func computeDistributeAmount(hbdAvg int64, amount int64, totalAvg int64) (int64, bool) {
	if totalAvg == 0 {
		return 0, false
	}
	out := new(big.Int).Mul(big.NewInt(hbdAvg), big.NewInt(amount))
	out.Quo(out, big.NewInt(totalAvg))
	if !out.IsInt64() {
		return 0, false
	}
	return out.Int64(), true
}
