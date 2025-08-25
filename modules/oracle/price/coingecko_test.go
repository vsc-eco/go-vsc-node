package price

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCoinGeckoHandlerQueryCoins(t *testing.T) {
	var (
		apiKey   = os.Getenv("COINGECKO_API_KEY")
		demoMode = true
	)

	cgHandler := makeCoinGeckoHandler(apiKey, demoMode, "usd")
	c := make(chan []observePricePoint, 10)

	symbols := []string{"BTC", "eth", "lTc"}

	cgHandler.QueryMarketPrice(symbols, c)

	result := <-c
	assert.Equal(t, len(symbols), len(result))

	for i, symbol := range symbols {
		t.Log(result[i])
		expectedSymbol := strings.ToUpper(symbol)
		assert.Equal(t, expectedSymbol, result[i].symbol)
	}
}
