package price

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"

	"github.com/libp2p/go-libp2p/core/peer"
)

var (
	errApiKeyNotFound = errors.New("API key not exported")

	_ p2p.MessageHandler = &PriceOracle{}
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
	logger      *slog.Logger
	avgPriceMap priceMap
	priceAPIs   map[string]priceQuery

	PricePoints    *threadsafe.LockedConsumer[PricePointMap]
	SignedBlocks   *threadsafe.LockedConsumer[p2p.OracleBlock]
	ProducerBlocks *threadsafe.LockedConsumer[p2p.OracleBlock]
}

func New(logger *slog.Logger, userCurrency string) (*PriceOracle, error) {
	priceQueryMap := map[string]priceQuery{
		"CoinMarketCap": &coinMarketCapHandler{},
		"CoinGecko":     &coinGeckoHandler{},
	}

	for src, api := range priceQueryMap {
		if err := api.initialize(userCurrency); err != nil {
			return nil, fmt.Errorf(
				"failed to initialize %s handler: %w",
				src, err,
			)
		}
	}

	p := &PriceOracle{
		logger:      logger.With("sub-service", "price-oracle"),
		avgPriceMap: priceMap{threadsafe.NewMap[string, pricePointData]()},
		priceAPIs:   priceQueryMap,

		PricePoints:    threadsafe.NewLockedConsumer[PricePointMap](256),
		SignedBlocks:   threadsafe.NewLockedConsumer[p2p.OracleBlock](256),
		ProducerBlocks: threadsafe.NewLockedConsumer[p2p.OracleBlock](8),
	}

	// locking states
	p.PricePoints.Lock()
	p.SignedBlocks.Lock()
	p.ProducerBlocks.Lock()

	return p, nil
}

func (p *PriceOracle) MarketObserve(
	ctx context.Context,
	interval time.Duration,
	symbols []string,
) {
	pricePollTicker := time.NewTicker(interval)

	for {
		select {
		case <-ctx.Done():
			return

		case <-pricePollTicker.C:
			p.queryMarket(symbols)
		}
	}
}

func (p *PriceOracle) queryMarket(symbols []string) {
	wg := &sync.WaitGroup{}
	wg.Add(len(p.priceAPIs))

	for src, api := range p.priceAPIs {
		go func(src string, api priceQuery) {
			defer wg.Done()

			pricePoints, err := api.queryMarketPrice(symbols)
			if err != nil {
				p.logger.Error("failed to query market", "src", src, "err", err)
				return
			}

			p.avgPriceMap.Observe(pricePoints)
			p.logger.Debug("market price fetched", "src", src)
		}(src, api)
	}

	wg.Wait()
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

// Handle implements p2p.MessageHandler.
func (o *PriceOracle) Handle(peerID peer.ID, msg p2p.Msg) (p2p.Msg, error) {
	var response p2p.Msg
	switch msg.Code {
	case p2p.MsgPriceBroadcast:
		data, err := httputils.JsonUnmarshal[map[string]p2p.AveragePricePoint](
			msg.Data,
		)
		if err != nil {
			return nil, err
		}

		pricePoints := collectPricePoint(peerID, data)
		if err := o.PricePoints.Consume(pricePoints); err != nil {
			if errors.Is(err, threadsafe.ErrLockedChannel) {
				o.logger.Debug(
					"unable to collect broadcasted average price points in the current block interval.",
				)
			} else {
				o.logger.Error("failed to collect pricePoint", "err", err)
			}
		}

	case p2p.MsgPriceSignature:
		block, err := httputils.JsonUnmarshal[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		block.TimeStamp = time.Now().UTC()
		if err := o.SignedBlocks.Consume(*block); err != nil {
			if errors.Is(err, threadsafe.ErrLockedChannel) {
				o.logger.Debug(
					"unable to collect broadcasted signatures in the current block interval.",
				)
			} else {
				o.logger.Error("failed to collect signatures", "err", err)
			}
		}

	case p2p.MsgPriceBlock:
		block, err := httputils.JsonUnmarshal[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		block.TimeStamp = time.Now().UTC()
		if err := o.ProducerBlocks.Consume(*block); err != nil {
			if errors.Is(err, threadsafe.ErrLockedChannel) {
				o.logger.Debug(
					"unable to collect and verify price block in the current block interval.",
				)
			} else {
				o.logger.Error("failed to collect price block", "err", err)
			}
		}

	default:
		return nil, p2p.ErrInvalidMessageType
	}

	return response, nil
}

func collectPricePoint(
	peerID peer.ID,
	data *map[string]p2p.AveragePricePoint,
) PricePointMap {
	recvTimeStamp := time.Now().UTC()

	m := make(PricePointMap)
	for symbol, avgPricePoint := range *data {
		sym := strings.ToUpper(symbol)

		pp := PricePoint{
			Price:       avgPricePoint.Price,
			Volume:      avgPricePoint.Volume,
			PeerID:      peerID,
			CollectedAt: recvTimeStamp,
		}

		v, ok := m[sym]
		if !ok {
			v = []PricePoint{pp}
		} else {
			v = append(v, pp)
		}

		m[sym] = v
	}

	return m
}
