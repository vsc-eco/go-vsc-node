package oracle

import (
	"math"
	"slices"
)

const (
	hourInSecond = 3600

	float64Epsilon = 1e-9
)

func getMedian(buf []float64) float64 {
	if len(buf) == 0 {
		return 0
	}

	slices.Sort(buf)

	evenCount := len(buf)&1 == 0
	if evenCount {
		i := len(buf) / 2
		return (buf[i] + buf[i-1]) / 2
	} else {
		return buf[len(buf)/2]
	}
}

func float64Eq(a, b float64) bool {
	return math.Abs(a-b) < float64Epsilon
}
