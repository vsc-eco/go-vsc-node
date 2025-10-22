package price

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/oracle/p2p"

	"github.com/chebyrash/promise"
)

var (
	errApiKeyNotFound = errors.New("API key not exported")

	_ p2p.MessageHandler = &PriceOracle{}
	_ aggregate.Plugin   = &PriceOracle{}
)

type PriceQuery interface {
	Source() string
	Initialize(string) error
	Query([]string) (map[string]PricePoint, error)
}

type PriceOracle struct {
	ctx               context.Context
	userCurrency      string
	pricePollInterval time.Duration
	watchSymbols      []string
	logger            *slog.Logger
	priceMap          *PriceMap
	priceAPIs         map[string]PriceQuery
	priceChannel      *PriceChannel
	conf              common.IdentityConfig
}

func New(
	ctx context.Context,
	oracleLogger *slog.Logger,
	userCurrency string,
	pricePollInterval time.Duration,
	watchSymbols []string,
	conf common.IdentityConfig,
) *PriceOracle {
	logger := oracleLogger.With("service", "price-oracle")

	return &PriceOracle{
		ctx:               ctx,
		userCurrency:      userCurrency,
		pricePollInterval: pricePollInterval,
		watchSymbols:      watchSymbols,
		logger:            logger,
		priceMap:          nil,
		priceAPIs:         nil,
		priceChannel:      nil,
		conf:              conf,
	}
}

// Init implements aggregate.Plugin.
func (p *PriceOracle) Init() error {
	p.priceMap = &PriceMap{
		buf: make(map[string]AveragePricePoint),
		mtx: &sync.Mutex{},
	}

	p.priceChannel = makePriceChannel()

	p.priceAPIs = map[string]PriceQuery{
		"CoinMarketCap": &CoinMarketCap{},
		"CoinGecko":     &CoinGecko{},
	}

	// initializes market api's
	for src, api := range p.priceAPIs {
		if err := api.Initialize(p.userCurrency); err != nil {
			return fmt.Errorf(
				"failed to initialize %s handler: %w",
				src, err,
			)
		}
		p.logger.Info("api source initialized", "source", src)
	}

	return nil
}

// Start implements aggregate.Plugin.
func (p *PriceOracle) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		p.logger.Debug("starting priceOracle service")

		go func() {
			pricePollTicker := time.NewTicker(p.pricePollInterval)

			for {
				select {
				case <-p.ctx.Done():
					return

				case <-pricePollTicker.C:
					for src, api := range p.priceAPIs {
						go func(src string, api PriceQuery) {

							pricePoints, err := api.Query(p.watchSymbols)
							if err != nil {
								p.logger.Error("failed to query market", "src", src, "err", err)
								return
							}

							p.priceMap.Observe(pricePoints)
							p.logger.Debug("market price fetched", "src", src)
						}(src, api)
					}
				}
			}
		}()

		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (p *PriceOracle) Stop() error {
	return nil
}
