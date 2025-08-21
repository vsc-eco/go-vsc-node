package price

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCoinGeckoHandlerQueryCoins(t *testing.T) {
	var (
		apiKey   = os.Getenv("COINGECKO_API_KEY")
		demoMode = true
	)

	cgHandler := makeCoinGeckoHandler(apiKey, demoMode, "cad")
	c := make(chan PricePoint, 10)

	go func() {
		for i := range c {
			t.Log(i)
		}
	}()

	assert.NoError(t, cgHandler.queryCoins(c, 30*time.Second))
}
