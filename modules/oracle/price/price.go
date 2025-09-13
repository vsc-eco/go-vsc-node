package price

import (
	"errors"
	"fmt"
	"vsc-node/modules/oracle/p2p"
)

var (
	errApiKeyNotFound = errors.New("API key not exported")
)

type PriceQuery interface {
	QueryMarketPrice([]string) (map[string]p2p.ObservePricePoint, error)
}

type PriceOracle struct {
	c           chan p2p.AveragePricePoint
	avgPriceMap priceMap

	PriceAPIs []PriceQuery
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

	priceQueries := []PriceQuery{coinGeckoHandler, coinMarketCapHanlder}

	p := &PriceOracle{
		c:           make(chan p2p.AveragePricePoint, 1),
		avgPriceMap: makePriceMap(),
		PriceAPIs:   priceQueries,
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

func (p *PriceOracle) ResetPriceCache() {
	p.avgPriceMap.priceSymbolMap = make(priceSymbolMap)
}
