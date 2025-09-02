package price

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
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
	coinGecko     PriceQuery
	coinMarketCap PriceQuery
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
		coinGecko:     coinGeckoHandler,
		coinMarketCap: coinMarketCapHanlder,
	}

	return p, nil
}

func (p *PriceOracle) Poll(
	ctx context.Context,
	broadcastInterval time.Duration,
	msgChan chan<- p2p.Msg,
	observePriceChan chan []p2p.ObservePricePoint,
) {
	var (
		priceBroadcastTicker = time.NewTicker(broadcastInterval)
		pricePollTicker      = time.NewTimer(time.Second * 15)
	)

	for {
		select {
		case <-ctx.Done():
			return

		case <-pricePollTicker.C:
			go p.coinGecko.QueryMarketPrice(
				watchSymbols[:],
				observePriceChan,
				msgChan,
			)
			go p.coinMarketCap.QueryMarketPrice(
				watchSymbols[:],
				observePriceChan,
				msgChan,
			)

		case pricePoints := <-observePriceChan:
			for _, pricePoint := range pricePoints {
				p.avgPriceMap.observe(pricePoint)
			}

		case <-priceBroadcastTicker.C:
			// TODO: if this node is do an early continue and clear the cache
			buf := make([]*p2p.AveragePricePoint, len(watchSymbols))

			var err error
			for i, symbol := range watchSymbols {
				buf[i], err = p.avgPriceMap.getAveragePrice(symbol)
				if err != nil {
					log.Println("symbol not found in map:", symbol)
					continue
				}
			}

			msgChan <- &p2p.OracleMessage{
				Type: p2p.MsgOraclePriceBroadcast,
				Data: buf,
			}

			p.avgPriceMap.priceSymbolMap = make(priceSymbolMap) // clear cache
		}
	}
}
