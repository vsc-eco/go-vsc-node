package price

import (
	"math"
	"math/rand"
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
		const (
			testPricePointCount = 0xffff
			priceLimit          = 10_000.00
		)

		var (
			pm  = makePriceMap()
			sum = float64(0.0)
		)

		for range testPricePointCount {
			pt := rand.Float64() * priceLimit
			sum += pt
			pm.observe(p2p.ObservePricePoint{
				Symbol: symbol,
				Price:  pt,
				Volume: pt,
			})
		}

		expectedAverage := sum / float64(testPricePointCount)

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

		t.Run("valid time stamp insertion", func(t *testing.T) {
			assert.NotEqual(t, 0, pricePoint.UnixTimeStamp)
		})

		t.Run("valid symbol insertion", func(t *testing.T) {
			assert.Equal(t, symbol, pricePoint.Symbol)
		})
	})

	t.Run("median price", func(t *testing.T) {
		t.Run("odd price points", func(t *testing.T) {
			var (
				pm     = makePriceMap()
				prices = []float64{0, 1, 4, 5, 2}
			)

			for _, price := range prices {
				pm.observe(p2p.ObservePricePoint{
					Symbol: symbol,
					Price:  price,
					Volume: 0,
				})
			}

			avgPrice, err := pm.getAveragePrice(symbol)
			assert.NoError(t, err)
			assert.Equal(t, prices[4], avgPrice.MedianPrice)
		})

		t.Run("even price points", func(t *testing.T) {
			var (
				pm     = makePriceMap()
				prices = []float64{0, 1, 4, 2}
			)

			for _, price := range prices {
				pm.observe(p2p.ObservePricePoint{
					Symbol: symbol,
					Price:  price,
					Volume: 0,
				})
			}

			avgPrice, err := pm.getAveragePrice(symbol)
			assert.NoError(t, err)
			assert.Equal(t, (prices[1]+prices[3])/2, avgPrice.MedianPrice)
		})
	})
}
