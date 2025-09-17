package price

import (
	"errors"
	"fmt"
	"log/slog"
	"vsc-node/modules/oracle/p2p"
)

var (
	errApiKeyNotFound = errors.New("API key not exported")
)

type PriceQuery interface {
	QueryMarketPrice([]string) (map[string]p2p.ObservePricePoint, error)
}

type PriceOracle struct {
	Logger      *slog.Logger
	AvgPriceMap priceMap
	PriceAPIs   []PriceQuery
}

func New(logger *slog.Logger, userCurrency string) (*PriceOracle, error) {
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
		Logger:      logger.With("sub-service", "price-oracle"),
		AvgPriceMap: makePriceMap(),
		PriceAPIs:   priceQueries,
	}

	return p, nil
}
