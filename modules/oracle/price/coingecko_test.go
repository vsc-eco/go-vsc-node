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

func Test_paginateIds(t *testing.T) {
	ids := []string{"BTC", "LTC", "ETH", "FOO", "BAR"}
	pageLimit := 2
	result := paginateIds(ids, pageLimit)

	assert.Equal(t, 3, len(result))
	assert.Equal(t, 2, len(result[0]))
	assert.Equal(t, 2, len(result[1]))
	assert.Equal(t, 1, len(result[2]))

	buf := make([]string, 0, 5)
	buf = append(buf, result[0]...)
	buf = append(buf, result[1]...)
	buf = append(buf, result[2]...)
	assert.Equal(t, ids, buf)
}
