package price

import (
	"os"
	"slices"
	"strings"
	"testing"
	"vsc-node/lib/utils"

	"github.com/stretchr/testify/assert"
)

func TestCoinGeckoHandlerQueryCoins(t *testing.T) {
	var (
		apiKey          = os.Getenv("COINGECKO_API_KEY")
		demoMode        = true
		cgHandler       = makeCoinGeckoHandler(apiKey, demoMode, "usd")
		c               = make(chan []observePricePoint, 10)
		symbols         = [...]string{"BTC", "eth", "lTc"}
		expectedSymbols = utils.Map(symbols[:], strings.ToUpper)
	)

	cgHandler.QueryMarketPrice(symbols[:], c)

	results := <-c
	assert.Equal(t, len(expectedSymbols), len(results))

	for _, observed := range results {
		t.Log(observed)
		assert.True(t, slices.Contains(expectedSymbols, observed.symbol))
	}
}
