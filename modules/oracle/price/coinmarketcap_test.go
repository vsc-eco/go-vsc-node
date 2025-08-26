package price

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCoinMarketCapQueryPrice(t *testing.T) {
	cmc, err := makeCoinMarketCapHandler()
	assert.NoError(t, err)

	watchSymbols := [...]string{"BTC", "ETH", "LTC"}
	c := make(chan []observePricePoint, 10)

	cmc.QueryMarketPrice(watchSymbols[:], c)

	result := <-c
	assert.Equal(t, len(watchSymbols), len(result))
	for i, symbol := range watchSymbols {
		t.Log(result[i])
		expectedSymbol := strings.ToUpper(symbol)
		assert.Equal(t, expectedSymbol, result[i].symbol)
	}
}
