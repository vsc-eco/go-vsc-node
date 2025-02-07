package hive

import (
	"math"
	"strconv"
)

// Converts Integer amount to string format
// For example 1 HIVE -> "0.001 HIVE" (precision 3)
func AmountToString(amount int64) string {
	amt := float64(amount) / math.Pow10(3)
	fAmt := strconv.FormatFloat(amt, 'f', -1, 64)

	return fAmt
}
