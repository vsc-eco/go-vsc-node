package price

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCoinGeckoHandlerQueryCoins(t *testing.T) {
	var (
		symbols = [...]string{"BTC", "eth", "lTc"}
	)

	os.Setenv("COINGECKO_API_DEMO", "1")
	cgHandler, err := makeCoinGeckoHandler("usd")
	assert.NoError(t, err)

	result, err := cgHandler.QueryMarketPrice(symbols[:])
	assert.NoError(t, err)
	t.Log(result)
}
