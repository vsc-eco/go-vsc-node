package price

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"

	"github.com/chebyrash/promise"
	"github.com/libp2p/go-libp2p/core/peer"
)

var (
	errApiKeyNotFound = errors.New("API key not exported")

	_ p2p.MessageHandler = &PriceOracle{}
	_ aggregate.Plugin   = &PriceOracle{}
)

type priceQuery interface {
	initialize(string) error
	queryMarketPrice([]string) (map[string]p2p.ObservePricePoint, error)
}

type PricePoint struct {
	Price  float64
	Volume float64
	PeerID peer.ID

	// unix UTC timestamp
	CollectedAt time.Time
}

type PricePointMap map[string][]PricePoint

type PriceOracle struct {
	pricePoints    *threadsafe.LockedConsumer[PricePointMap]
	signedBlocks   *threadsafe.LockedConsumer[p2p.OracleBlock]
	producerBlocks *threadsafe.LockedConsumer[p2p.OracleBlock]

	userCurrency      string
	pricePollInterval time.Duration
	watchSymbols      []string

	ctx         context.Context
	logger      *slog.Logger
	avgPriceMap priceMap
	priceAPIs   map[string]priceQuery
	conf        common.IdentityConfig
}

func New(
	ctx context.Context,
	oracleLogger *slog.Logger,
	userCurrency string,
	pricePollInterval time.Duration,
	watchSymbols []string,
	conf common.IdentityConfig,
) *PriceOracle {
	var (
		logger         = oracleLogger.With("sub-service", "price-oracle")
		pricePoints    = threadsafe.NewLockedConsumer[PricePointMap](256)
		signedBlocks   = threadsafe.NewLockedConsumer[p2p.OracleBlock](256)
		producerBlocks = threadsafe.NewLockedConsumer[p2p.OracleBlock](8)
		avgPriceMap    = priceMap{threadsafe.NewMap[string, pricePointData]()}
		priceQueryMap  = map[string]priceQuery{
			"CoinMarketCap": &coinMarketCapHandler{},
			"CoinGecko":     &coinGeckoHandler{},
		}
	)

	return &PriceOracle{
		pricePoints:       pricePoints,
		signedBlocks:      signedBlocks,
		producerBlocks:    producerBlocks,
		userCurrency:      userCurrency,
		pricePollInterval: pricePollInterval,
		logger:            logger,
		avgPriceMap:       avgPriceMap,
		priceAPIs:         priceQueryMap,
		conf:              conf,
	}
}

// Init implements aggregate.Plugin.
func (p *PriceOracle) Init() error {
	// locking states
	p.pricePoints.Lock()
	p.signedBlocks.Lock()
	p.producerBlocks.Lock()

	// initializes market api's
	for src, api := range p.priceAPIs {
		if err := api.initialize(p.userCurrency); err != nil {
			return fmt.Errorf(
				"failed to initialize %s handler: %w",
				src, err,
			)
		}
	}

	return nil
}

// Start implements aggregate.Plugin.
func (p *PriceOracle) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		p.logger.Debug("starting priceOracle service")
		go p.marketObserve()
		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (p *PriceOracle) Stop() error {
	return nil
}

func (p *PriceOracle) GetLocalQueriedAveragePrices() map[string]p2p.AveragePricePoint {
	out := make(map[string]p2p.AveragePricePoint)

	for symbol, p := range p.avgPriceMap.Get() {
		symbol = strings.ToUpper(symbol)
		out[symbol] = p2p.MakeAveragePricePoint(p.avgPrice, p.avgVolume)
	}

	return out
}

func (p *PriceOracle) ClearPriceCache() {
	p.avgPriceMap.Clear()
}
