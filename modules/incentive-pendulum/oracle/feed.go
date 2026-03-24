package oracle

import "math"

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

// SimpleMean returns the arithmetic mean of values; ok is false if values is empty.
func SimpleMean(values []float64) (mean float64, ok bool) {
	if len(values) == 0 {
		return 0, false
	}
	var s float64
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		s += v
	}
	return s / float64(len(values)), true
}

// TrustedHivePrice aggregates trusted witness quotes (HIVE priced in HBD) with a simple mean.
// Only include quotes for witnesses where trusted[i] is true.
func TrustedHivePrice(quotes map[string]float64, trusted map[string]bool) (price float64, ok bool) {
	if len(quotes) == 0 || len(trusted) == 0 {
		return 0, false
	}
	var vals []float64
	for w, q := range quotes {
		if !trusted[w] {
			continue
		}
		if q <= 0 || math.IsNaN(q) || math.IsInf(q, 0) {
			continue
		}
		vals = append(vals, q)
	}
	if len(vals) == 0 {
		return 0, false
	}
	return SimpleMean(vals)
}
