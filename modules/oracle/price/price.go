package price

import (
	"errors"
	"fmt"
	"vsc-node/modules/oracle/p2p"
)

var (
	errApiKeyNotFound = errors.New("API key not exported")
	watchSymbols      = [...]string{"BTC", "ETH", "LTC"}
)

type PriceQuery interface {
	QueryMarketPrice([]string, chan<- []p2p.ObservePricePoint, chan<- p2p.Msg)
}

type PriceOracle struct {
	c             chan p2p.AveragePricePoint
	avgPriceMap   priceMap
	CoinGecko     PriceQuery
	CoinMarketCap PriceQuery
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
		c:             make(chan p2p.AveragePricePoint, 1),
		avgPriceMap:   makePriceMap(),
		CoinGecko:     coinGeckoHandler,
		CoinMarketCap: coinMarketCapHanlder,
	}

	return p, nil
}

func (p *PriceOracle) ObservePricePoint(pricePoints []p2p.ObservePricePoint) {
	for _, pricePoint := range pricePoints {
		p.avgPriceMap.observe(pricePoint)
	}
}

func (p *PriceOracle) GetAveragePrice(
	symbol string,
) (*p2p.AveragePricePoint, error) {
	return p.avgPriceMap.getAveragePrice(symbol)
}
