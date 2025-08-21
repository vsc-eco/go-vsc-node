package price

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/go-playground/validator/v10"
)

var priceValidator = validator.New(validator.WithRequiredStructEnabled())

const (
	coingeckoApiRootUrl = "https://pro-api.coingecko.com/api/v3"
)

type (
	PriceOracle struct {
		c           chan PricePoint
		avgPriceMap priceMap
		coinGecko   coinGeckoHandler // TODO: make this into an interface for coinmarketcap
		// TODO: query both coinmarketcap and coingecko then average them
	}

	PricePoint struct {
		// length: range from 1-9 chars.
		// format: uppercase letters, may include numbers.
		Symbol        string  `json:"symbol"          validate:"required,min=1,max=9,alphanum"` // no need to validate
		Price         float64 `json:"current_price"   validate:"required,gt=0.0"`
		UnixTimeStamp int64   `json:"unix_time_stamp"`
	}
)

// UnmarshalJSON implements json.Unmarshaler
func (p *PricePoint) UnmarshalJSON(data []byte) error {
	type alias *PricePoint
	buf := (alias)(p)

	if err := json.Unmarshal(data, buf); err != nil {
		return err
	}

	// return priceValidator.Struct(p)
	return nil
}

func New() PriceOracle {
	var (
		demoMode bool = false

		// these need to be loaded from user config
		apiKey     string
		vsCurrency string
	)

	return PriceOracle{
		c:           make(chan PricePoint, 1),
		avgPriceMap: makePriceMap(),
		coinGecko:   makeCoinGeckoHandler(apiKey, demoMode, vsCurrency),
	}
}

// poll eveyr 10mins
func (p *PriceOracle) Poll(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)

	c := make(chan PricePoint, 10)
	go func() {
		if err := p.coinGecko.queryCoins(ctx, c, pollInterval); err != nil {
			log.Println(err)
		}
	}()

	select {
	case <-ctx.Done():
		return

	case <-ticker.C:
		p.fetchPrices()
	}
}

func (p *PriceOracle) fetchPrices() {
	fmt.Println("TODO: implement PriceOracle.fetchPrices()")
}
