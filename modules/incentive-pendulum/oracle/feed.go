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

// ToSQ64 returns the SQ64 fixed-point form of HBD per 1 HIVE for this quote
// (HbdRaw * SQ64Scale / HiveRaw, big.Int internally to avoid overflow).
// Returns 0 when HiveRaw is zero — caller is expected to filter those out
// upstream via the trusted-witness predicate.
func (q Quote) ToSQ64() intmath.SQ64 {
	if q.HiveRaw <= 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(q.HbdRaw), big.NewInt(intmath.SQ64Scale))
	num.Quo(num, big.NewInt(q.HiveRaw))
	return intmath.SQ64(num.Int64())
}

// TrustedHivePriceSQ64 is the deterministic mean HIVE price (HBD per HIVE,
// SQ64 fixed-point) across trusted witnesses. Iterates witness names in
// lexicographic order, converts each Quote to SQ64, and integer-divides the
// big.Int sum by the count. Bit-equal across nodes given the same on-chain
// inputs — replaces the prior float aggregator that summed map values in
// random Go map iteration order.
func TrustedHivePriceSQ64(quotes map[string]Quote, trusted map[string]bool) (intmath.SQ64, bool) {
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
	for _, w := range names {
		sum.Add(sum, big.NewInt(int64(quotes[w].ToSQ64())))
	}
	mean := new(big.Int).Quo(sum, big.NewInt(int64(len(names))))
	return intmath.SQ64(mean.Int64()), true
}
