package price

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price/api"

	"github.com/chebyrash/promise"
)

var (
	_ p2p.MessageHandler = &PriceOracle{}
	_ aggregate.Plugin   = &PriceOracle{}

	_ api.PriceQuery = &api.CoinGecko{}
	_ api.PriceQuery = &api.CoinMarketCap{}
)

type PriceOracle struct {
	ctx               context.Context
	userCurrency      string
	pricePollInterval time.Duration
	watchSymbols      []string
	logger            *slog.Logger
	conf              common.IdentityConfig

	priceMap  *PriceMap
	priceAPIs map[string]api.PriceQuery

	priceChannel       *PriceChannel
	sigRequestChannel  *SignatureRequestChannel
	sigResponseChannel *SignatureResponseChannel
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
		conf:              conf,
		priceMap:          nil,
		priceAPIs: map[string]api.PriceQuery{
			"CoinMarketCap": &api.CoinMarketCap{},
			"CoinGecko":     &api.CoinGecko{},
		},
		priceChannel:       MakeThreadChan[map[string]api.PricePoint](),
		sigRequestChannel:  MakeThreadChan[SignatureRequestMessage](),
		sigResponseChannel: MakeThreadChan[SignatureResponseMessage](),
	}
}

// Init implements aggregate.Plugin.
func (p *PriceOracle) Init() error {
	p.priceMap = &PriceMap{
		buf: make(map[string]AveragePricePoint),
		mtx: &sync.Mutex{},
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

			p.logger.Debug("observing market price", "symbols", p.watchSymbols)
			for {
				select {
				case <-p.ctx.Done():
					return

				case <-pricePollTicker.C:
					p.logger.Debug("price query ticker")

					for name, src := range p.priceAPIs {
						go func(src string, api api.PriceQuery) {
							p.logger.Debug("fetching price", "src", src)

							pricePoints, err := api.Query(p.watchSymbols)
							if err != nil {
								p.logger.Error("failed to query market", "src", src, "err", err)
								return
							}

							p.priceMap.Observe(pricePoints)
							p.logger.Debug("market price fetched", "src", src, "prices", p.priceMap.buf)
						}(name, src)
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
