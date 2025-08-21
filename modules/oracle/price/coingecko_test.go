package price

import (
	"context"
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

	cgHandler := makeCoinGeckoHandler(apiKey, demoMode, "usd")
	c := make(chan PricePoint, 10)

	go func() {
		for i := range c {
			t.Log(i)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	assert.NoError(
		t,
		cgHandler.queryCoins(ctx, c, 30*time.Second),
	)
}
