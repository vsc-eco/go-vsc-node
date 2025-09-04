package price

import (
	"os"
	"slices"
	"strings"
	"testing"
	"vsc-node/lib/utils"
	"vsc-node/modules/oracle/p2p"

	"github.com/stretchr/testify/assert"
)

func TestCoinGeckoHandlerQueryCoins(t *testing.T) {
	var (
		priceChan       = make(chan []p2p.ObservePricePoint, 10)
		symbols         = [...]string{"BTC", "eth", "lTc"}
		expectedSymbols = utils.Map(symbols[:], strings.ToUpper)
	)

	os.Setenv("COINGECKO_API_DEMO", "1")
	cgHandler, err := makeCoinGeckoHandler("usd")
	assert.NoError(t, err)

	cgHandler.QueryMarketPrice(symbols[:], priceChan)

	results := <-priceChan
	assert.Equal(t, len(expectedSymbols), len(results))

	for _, observed := range results {
		t.Log(observed.String())
		assert.True(t, slices.Contains(expectedSymbols, observed.Symbol))
	}
}
