package price

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-playground/validator/v10"
)

var (
	priceValidator = validator.New(validator.WithRequiredStructEnabled())
	watchSymbols   = [...]string{"BTC", "ETH", "LTC"}
)

const (
	coingeckoApiRootUrl = "https://pro-api.coingecko.com/api/v3"
)

type PriceQuery interface {
	QueryMarketPrice([]string, chan<- []PricePoint)
}

type PriceOracle struct {
	c             chan PricePoint
	avgPriceMap   priceMap
	coinGecko     PriceQuery // TODO: make this into an interface for coinmarketcap
	coinMarketCap PriceQuery
	// TODO: query both coinmarketcap and coingecko then average them
}

type PricePoint struct {
	// length: range from 1-9 chars.
	// format: uppercase letters, may include numbers.
	Symbol        string  `json:"symbol"                    validate:"required,min=1,max=9,alphanum"` // no need to validate
	Price         float64 `json:"current_price"             validate:"required,gt=0.0"`
	UnixTimeStamp int64   `json:"unix_time_stamp,omitempty"`
}

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
func (p *PriceOracle) Poll(
	ctx context.Context,
	broadcastInterval time.Duration,
	broadcastChannel chan<- []PricePoint,
) {
	priceBroadcastTicker := time.NewTicker(broadcastInterval)
	pricePollTicker := time.NewTimer(time.Hour)

	priceChan := make(chan []PricePoint, 10)

	for {
		select {
		case <-ctx.Done():
			return

		case <-pricePollTicker.C:
			go p.coinGecko.QueryMarketPrice(watchSymbols[:], priceChan)
			go p.coinMarketCap.QueryMarketPrice(watchSymbols[:], priceChan)

		case pricePoints := <-priceChan:
			for _, pricePoint := range pricePoints {
				p.avgPriceMap.observe(pricePoint)
			}

		case <-priceBroadcastTicker.C:
			now := time.Now().UTC().Unix()
			pp := make([]PricePoint, 0, len(watchSymbols))

			for symbol, avgPrice := range p.avgPriceMap.priceSymbolMap {
				p := PricePoint{
					Symbol:        symbol,
					Price:         avgPrice.average,
					UnixTimeStamp: now,
				}
				pp = append(pp, p)
			}

			broadcastChannel <- pp
			p.avgPriceMap.priceSymbolMap = make(priceSymbolMap)
		}
	}
}
