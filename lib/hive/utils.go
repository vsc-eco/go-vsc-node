package hive

import (
	"strconv"
)

// Converts Integer amount to string format
// For example 1 HIVE -> "0.001 HIVE" (precision 3)
func AmountToString(amount int64) string {
	// amt := float64(amount) / math.Pow10(3)
	// fAmt := strconv.FormatFloat(amt, 'f', -1, 64)
	str := strconv.Itoa(int(amount))
	var intAmt string
	if len(str) > 3 {
		intAmt = str[:len(str)-3]
	} else {
		intAmt = "0"
	}
	var decAmt = str
	if len(str) < 3 {
		for i := 0; i < 3-len(str); i++ {
			decAmt = "0" + decAmt
		}
	} else {
		decAmt = str[len(str)-3:]
	}

	amt := intAmt + "." + decAmt

	return amt
}
