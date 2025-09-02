package price

import (
	"slices"
	"strings"
	"testing"
	"vsc-node/lib/utils"
	"vsc-node/modules/oracle/p2p"

	"github.com/stretchr/testify/assert"
)

func TestCoinMarketCapQueryPrice(t *testing.T) {
	cmc, err := makeCoinMarketCapHandler("usd")
	assert.NoError(t, err)

	var (
		watchSymbols    = [...]string{"BTC", "ETH", "LTC"}
		expectedSymbols = utils.Map(watchSymbols[:], strings.ToUpper)
		priceChan       = make(chan []p2p.ObservePricePoint, 10)
		msgChan         = make(chan p2p.Msg, 10)
	)

	cmc.QueryMarketPrice(watchSymbols[:], priceChan, msgChan)

	results := <-priceChan
	assert.Equal(t, len(watchSymbols), len(results))

	for _, observed := range results {
		t.Log(observed)
		assert.True(t, slices.Contains(expectedSymbols, observed.Symbol))
	}
}
