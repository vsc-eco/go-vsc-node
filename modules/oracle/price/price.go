package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-playground/validator/v10"
)

var (
	errApiKeyNotFound = errors.New("API key not exported")
	priceValidator    = validator.New(validator.WithRequiredStructEnabled())
	watchSymbols      = [...]string{"BTC", "ETH", "LTC"}
)

type PriceQuery interface {
	QueryMarketPrice([]string, chan<- []observePricePoint)
}

type PriceOracle struct {
	c             chan AveragePricePoint
	avgPriceMap   priceMap
	coinGecko     PriceQuery // TODO: make this into an interface for coinmarketcap
	coinMarketCap PriceQuery
	// TODO: query both coinmarketcap and coingecko then average them
}

type AveragePricePoint struct {
	// length: range from 1-9 chars.
	// format: uppercase letters, may include numbers.
	Symbol        string  `json:"symbol"                    validate:"required,min=1,max=9,alphanum"` // no need to validate
	Price         float64 `json:"average_price"             validate:"required,gt=0.0"`
	MedianPrice   float64 `json:"median_price"              validate:"required,gt=0.0"`
	Volume        float64 `json:"average_volume"            validate:"required,gt=0.0"`
	UnixTimeStamp int64   `json:"unix_time_stamp,omitempty" validate:"required,gt=0"`
}

// UnmarshalJSON implements json.Unmarshaler
func (p *AveragePricePoint) UnmarshalJSON(data []byte) error {
	type alias *AveragePricePoint
	buf := (alias)(p)

	if err := json.Unmarshal(data, buf); err != nil {
		return err
	}

	return priceValidator.Struct(p)
}

func New(userCurrency string) (*PriceOracle, error) {
	coinMarketCapHanlder, err := makeCoinMarketCapHandler(userCurrency)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to initialized coinmarketcap api handler: %w",
			err,
		)
	}

	coinGeckoHandler, err := makeCoinGeckoHandler(userCurrency)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to initialize CoinGecko handler: %w",
			err,
		)
	}

	p := &PriceOracle{
		c:             make(chan AveragePricePoint, 1),
		avgPriceMap:   makePriceMap(),
		coinGecko:     coinGeckoHandler,
		coinMarketCap: coinMarketCapHanlder,
	}

	return p, nil
}

func (p *PriceOracle) Poll(
	ctx context.Context,
	broadcastInterval time.Duration,
	broadcastChannel chan<- []*AveragePricePoint,
) {
	priceBroadcastTicker := time.NewTicker(broadcastInterval)
	pricePollTicker := time.NewTimer(time.Hour)

	priceChan := make(chan []observePricePoint, 10)

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
			buf := make([]*AveragePricePoint, len(watchSymbols))

			var err error
			for i, symbol := range watchSymbols {
				buf[i], err = p.avgPriceMap.getAveragePrice(symbol)
				if err != nil {
					log.Println("symbol not found in map:", symbol)
					continue
				}
			}

			broadcastChannel <- buf
			p.avgPriceMap.priceSymbolMap = make(priceSymbolMap) // clear cache
		}
	}
}
