package oracle

import (
	"math/big"
	"sort"

	"vsc-node/lib/intmath"
)

const (
	// DefaultMinSignatures is the minimum blocks signed in the last window to trust a feed (pendulum plan).
	DefaultMinSignatures = 4
)

// FeedTrust returns true if the witness may contribute to the aggregate HIVE price:
// they published or refreshed a price update and signed at least minSig blocks in the window.
func FeedTrust(sigsInWindow int, publishedPriceUpdate bool, minSig int) bool {
	if minSig < 1 {
		minSig = DefaultMinSignatures
	}
	return publishedPriceUpdate && sigsInWindow >= minSig
}

// Quote is a witness's published HIVE price as the rational pair from a
// feed_publish op. HbdRaw and HiveRaw are base-unit integer amounts (HBD
// and HIVE both have L1 precision 3, so 1.234 HBD == 1234 raw). The HBD-per-HIVE
// price is HbdRaw / HiveRaw at consensus time; carrying the rational here
// instead of a precomputed float means every node sums the same integers in
// the same order without any IEEE-754 rounding entering the consensus path.
type Quote struct {
	HbdRaw  int64
	HiveRaw int64
}

// PriceBps returns the HBD-per-HIVE price for this quote in basis points
// (BpsScale = 1.0). Returns 0 when HiveRaw is zero — caller is expected to
// filter those out upstream via the trusted-witness predicate.
func (q Quote) PriceBps() int64 {
	if q.HiveRaw <= 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(q.HbdRaw), big.NewInt(intmath.BpsScale))
	num.Quo(num, big.NewInt(q.HiveRaw))
	if !num.IsInt64() {
		return 0
	}
	return num.Int64()
}

// TrustedHivePriceBps is the deterministic mean HIVE price (HBD per HIVE,
// basis points) across trusted witnesses. Iterates witness names in
// lexicographic order, computes each Quote.PriceBps in big.Int, sums, and
// integer-divides by the count. Bit-equal across nodes given the same on-chain
// inputs.
func TrustedHivePriceBps(quotes map[string]Quote, trusted map[string]bool) (int64, bool) {
	if len(quotes) == 0 || len(trusted) == 0 {
		return 0, false
	}
	names := make([]string, 0, len(quotes))
	for w, q := range quotes {
		if !trusted[w] {
			continue
		}
		if q.HbdRaw <= 0 || q.HiveRaw <= 0 {
			continue
		}
		names = append(names, w)
	}
	if len(names) == 0 {
		return 0, false
	}
	sort.Strings(names)
	sum := new(big.Int)
	bpsBig := big.NewInt(intmath.BpsScale)
	for _, w := range names {
		q := quotes[w]
		// Per-witness price in bps, computed in big.Int to avoid overflow on
		// HbdRaw * BpsScale before the divide.
		num := new(big.Int).Mul(big.NewInt(q.HbdRaw), bpsBig)
		num.Quo(num, big.NewInt(q.HiveRaw))
		sum.Add(sum, num)
	}
	mean := new(big.Int).Quo(sum, big.NewInt(int64(len(names))))
	if !mean.IsInt64() {
		return 0, false
	}
	return mean.Int64(), true
}

