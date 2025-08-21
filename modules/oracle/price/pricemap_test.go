package price

import (
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPriceMap(t *testing.T) {
	const (
		symbol              = "BTC"
		testPricePointCount = 0xffff
		precision           = 0x1p-27 // smallest precision with 8 decimal places
		priceLimit          = 10_000.00
	)

	var (
		pm  = makePriceMap()
		sum = float64(0.0)
	)

	for range testPricePointCount {
		pt := rand.Float64() * priceLimit
		sum += pt
		pm.observe(PricePoint{Symbol: symbol, Price: pt})
	}

	expectedAverage := sum / float64(testPricePointCount)

	calculatedAverage, err := pm.getAveragePrice(symbol)
	assert.NoError(t, err)

	diff := math.Abs(expectedAverage - calculatedAverage)
	assert.True(t, diff < precision)
	t.Log(diff, precision)
}
