package price

import (
	"slices"
	"strings"
	"testing"
	"vsc-node/lib/utils"

	"github.com/stretchr/testify/assert"
)

func TestCoinMarketCapQueryPrice(t *testing.T) {
	cmc, err := makeCoinMarketCapHandler("usd")
	assert.NoError(t, err)

	var (
		watchSymbols    = [...]string{"BTC", "ETH", "LTC"}
		expectedSymbols = utils.Map(watchSymbols[:], strings.ToUpper)
		c               = make(chan []observePricePoint, 10)
	)

	cmc.QueryMarketPrice(watchSymbols[:], c)

	results := <-c
	assert.Equal(t, len(watchSymbols), len(results))

	for _, observed := range results {
		t.Log(observed)
		assert.True(t, slices.Contains(expectedSymbols, observed.symbol))
	}
}
