package price

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCoinMarketCapQueryPrice(t *testing.T) {
	cmc, err := makeCoinMarketCapHandler("usd")
	assert.NoError(t, err)

	var (
		watchSymbols = [...]string{"BTC", "ETH", "LTC"}
	)

	result, err := cmc.QueryMarketPrice(watchSymbols[:])
	assert.NoError(t, err)
	t.Log(result)
}
