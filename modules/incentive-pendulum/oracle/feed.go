package oracle

import (
	"math/big"
	"sort"
	"strconv"

	"vsc-node/lib/intmath"
)

// NAI asset symbol codes. Shared across Hive networks — mainnet HBD/HIVE and
// testnet TBD/TESTS carry the same codes — so matching on NAI is unambiguous
// and needs no per-network symbol normalization.
const (
	naiHBD  = "@@000000013"
	naiHIVE = "@@000000021"
)

// quoteFromNAIExchangeRate builds a Quote from a feed_publish exchange_rate
// whose base/quote legs are NAI objects ({amount, nai, precision}) — the form
// the HAF block API emits, and what the rest of the VSC node already consumes
// (see the transfer handler in state_engine.go). The legs may be in either
// order. Returns (_, false) for legacy string-form legs so the caller can fall
// back to parseHbdPerHivePair.
func quoteFromNAIExchangeRate(er map[string]interface{}) (Quote, bool) {
	baseAmt, baseNai, ok1 := naiAssetRaw(er["base"])
	quoteAmt, quoteNai, ok2 := naiAssetRaw(er["quote"])
	if !ok1 || !ok2 {
		return Quote{}, false
	}
	switch {
	case baseNai == naiHBD && quoteNai == naiHIVE:
		return Quote{HbdRaw: baseAmt, HiveRaw: quoteAmt}, true
	case baseNai == naiHIVE && quoteNai == naiHBD:
		return Quote{HbdRaw: quoteAmt, HiveRaw: baseAmt}, true
	default:
		return Quote{}, false
	}
}

// naiAssetRaw extracts (base-unit amount, NAI code) from a NAI asset object.
// HBD and HIVE both carry L1 precision 3, so the raw amounts are directly
// comparable inside a Quote with no precision scaling.
func naiAssetRaw(v interface{}) (amount int64, nai string, ok bool) {
	m, isMap := v.(map[string]interface{})
	if !isMap {
		return 0, "", false
	}
	nai, _ = asString(m["nai"])
	if nai == "" {
		return 0, "", false
	}
	amtStr, _ := asString(m["amount"])
	amt, err := strconv.ParseInt(amtStr, 10, 64)
	if err != nil || amt <= 0 {
		return 0, "", false
	}
	return amt, nai, true
}

const (
	// DefaultMinBlocksProduced is the minimum L1 blocks a witness must have
	// produced inside the rolling production window to count toward the
	// trusted price mean. Combined with FeedTrust's published-recently gate,
	// this restricts contributors to actively-rotating Hive top-witnesses.
	DefaultMinBlocksProduced = 4
)

// FeedTrust returns true iff the witness may contribute to the aggregate
// HIVE price: they published or refreshed a price update in the trust
// window AND produced at least minBlocks of the last Width L1 blocks.
func FeedTrust(blocksInWindow int, publishedPriceUpdate bool, minBlocks int) bool {
	if minBlocks < 1 {
		minBlocks = DefaultMinBlocksProduced
	}
	return publishedPriceUpdate && blocksInWindow >= minBlocks
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

// TrustedHivePriceBps is the deterministic interquartile mean HIVE price
// (HBD per HIVE, basis points) across trusted witnesses. The bottom and
// top quartiles of the per-witness prices are dropped before averaging,
// so a colluding cluster pushing extreme values off any single side has to
// overwhelm 75% of the trusted set instead of 50% to move the result —
// while preserving mean-style smoothing across the middle 50%.
//
// Trim formula: drop floor(N/4) values from each end and mean the
// remainder. Examples:
//
//	N=1..3 → no trim, simple mean (too few points to define quartiles)
//	N=4    → keep middle 2 (drop 1 from each end)
//	N=8    → keep middle 4
//	N=20   → keep middle 10
//
// Determinism: the price list is sorted ascending. Equal prices have
// identical contribution to the sum and the trim, so sort.Slice's lack of
// stability doesn't affect the output. Bit-equal across nodes given the
// same on-chain inputs.
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
	// Iterate by sorted name to compute prices deterministically.
	sort.Strings(names)
	bpsBig := big.NewInt(intmath.BpsScale)
	prices := make([]*big.Int, 0, len(names))
	for _, w := range names {
		q := quotes[w]
		num := new(big.Int).Mul(big.NewInt(q.HbdRaw), bpsBig)
		num.Quo(num, big.NewInt(q.HiveRaw))
		prices = append(prices, num)
	}

	// Sort ascending by price for the interquartile trim.
	sort.Slice(prices, func(i, j int) bool {
		return prices[i].Cmp(prices[j]) < 0
	})

	n := len(prices)
	trim := n / 4 // drop floor(N/4) from each end
	kept := prices[trim : n-trim]
	if len(kept) == 0 {
		// Defensive — n/4*2 < n for any n >= 1 so kept is non-empty,
		// but guard anyway.
		return 0, false
	}

	sum := new(big.Int)
	for _, p := range kept {
		sum.Add(sum, p)
	}
	mean := new(big.Int).Quo(sum, big.NewInt(int64(len(kept))))
	if !mean.IsInt64() {
		return 0, false
	}
	return mean.Int64(), true
}
