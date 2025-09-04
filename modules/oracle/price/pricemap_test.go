package price

import (
	"math"
	"testing"
	"vsc-node/modules/oracle/p2p"

	"github.com/stretchr/testify/assert"
)

func TestPriceMap(t *testing.T) {
	const (
		symbol    = "BTC"
		precision = 0x1p-27 // smallest precision with 8 decimal places
	)

	t.Run("error on invalid symbol", func(t *testing.T) {
		pm := makePriceMap()
		_, err := pm.getAveragePrice("foo")
		assert.Error(t, err)
	})

	t.Run("observe and average", func(t *testing.T) {
		var (
			testPricePointCount = []int{5, 4, 5, 6, 5}
			pm                  = makePriceMap()
		)

		for _, p := range testPricePointCount {
			pm.observe(p2p.ObservePricePoint{
				Symbol: symbol,
				Price:  float64(p),
				Volume: float64(p),
			})
		}

		expectedAverage := float64(5)

		pricePoint, err := pm.getAveragePrice(symbol)
		assert.NoError(t, err)

		t.Run("average price", func(t *testing.T) {
			priceDiff := math.Abs(expectedAverage - pricePoint.Price)
			assert.True(t, priceDiff < precision)
		})

		t.Run("average volume", func(t *testing.T) {
			volumeDiff := math.Abs(expectedAverage - pricePoint.Volume)
			assert.True(t, volumeDiff < precision)
		})

		t.Run("valid symbol insertion", func(t *testing.T) {
			assert.Equal(t, symbol, pricePoint.Symbol)
		})
	})
}
