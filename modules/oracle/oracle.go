package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	btcrelay "vsc-node/modules/oracle/btc-relay"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price"
	libp2p "vsc-node/modules/p2p"

	"github.com/chebyrash/promise"
)

const (
	btcChainRelayInterval        = time.Minute * 10
	priceOracleBroadcastInterval = time.Hour
	priceOraclePollInterval      = time.Second * 15
)

var (
	watchSymbols = []string{"BTC", "ETH", "LTC"}
)

type Oracle struct {
	p2p        *libp2p.P2PServer
	service    libp2p.PubSubService[p2p.Msg]
	conf       common.IdentityConfig
	electionDb elections.Elections

	ctx        context.Context
	cancelFunc context.CancelFunc

	priceOracle      *price.PriceOracle
	observePriceChan chan []p2p.ObservePricePoint

	btcChainRelayer btcrelay.BtcChainRelay
	btcChan         chan *p2p.BtcHeadBlock

	msgChan chan p2p.Msg
}

func New(
	p2pServer *libp2p.P2PServer,
	conf common.IdentityConfig,
	electionDb elections.Elections,
) *Oracle {
	return &Oracle{
		p2p:              p2pServer,
		conf:             conf,
		electionDb:       electionDb,
		btcChan:          make(chan *p2p.BtcHeadBlock),
		msgChan:          make(chan p2p.Msg),
		observePriceChan: make(chan []p2p.ObservePricePoint, 10),
	}
}

// Init implements aggregate.Plugin.
// Runs initialization in order of how they are passed in to `Aggregate`
func (o *Oracle) Init() error {
	const userCurrency = "usd" // NOTE: only supporting USD for now

	var err error

	o.priceOracle, err = price.New(userCurrency)
	if err != nil {
		return fmt.Errorf("failed to initialize price oracle: %w", err)
	}

	o.btcChainRelayer = btcrelay.New(o.btcChan)

	o.ctx, o.cancelFunc = context.WithCancel(context.Background())

	return nil
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (o *Oracle) Start() *promise.Promise[any] {
	go o.marketObserve()

	return promise.New(func(resolve func(any), reject func(error)) {
		var err error

		o.service, err = libp2p.NewPubSubService(
			o.p2p,
			p2p.New(o.conf, o.observePriceChan, o.btcChan),
		)
		if err != nil {
			o.cancelFunc()
			reject(err)
			return
		}

		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
// Runs cleanup once the `Aggregate` is finished
func (o *Oracle) Stop() error {
	o.cancelFunc()

	if o.service == nil {
		return nil
	}
	return o.service.Close()
}

func (o *Oracle) marketObserve() {
	var (
		priceBroadcastTicker = time.NewTicker(priceOracleBroadcastInterval)
		pricePollTicker      = time.NewTicker(priceOraclePollInterval)
		btcChainRelayTicker  = time.NewTicker(btcChainRelayInterval)
	)

	for {
		select {
		case <-o.ctx.Done():
			return

		case <-pricePollTicker.C:
			go o.priceOracle.CoinGecko.QueryMarketPrice(
				watchSymbols,
				o.observePriceChan,
				o.msgChan,
			)

			go o.priceOracle.CoinMarketCap.QueryMarketPrice(
				watchSymbols,
				o.observePriceChan,
				o.msgChan,
			)

		case <-priceBroadcastTicker.C:
			go o.broadcastPricePoints()

		case <-btcChainRelayTicker.C:
			go o.relayBtcHeadBlock()

		case msg := <-o.msgChan:
			// TODO: which one to send broadcast within the network?
			if err := o.service.Send(msg); err != nil {
				log.Println("[oracle] failed broadcast message", msg, err)
				continue
			}

			jbytes, err := json.Marshal(msg)
			if err != nil {
				log.Println("[oracle] failed serialize message", msg, err)
				continue
			}
			o.p2p.SendToAll(p2p.OracleTopic, jbytes)

		case pricePoints := <-o.observePriceChan:
			o.priceOracle.ObservePricePoint(pricePoints)

		case btcHeadBlock := <-o.btcChan:
			fmt.Println("TODO: validate btcHeadBlock", btcHeadBlock)
		}
	}
}

func (o *Oracle) broadcastPricePoints() {
	// TODO: if this node is do an early continue and clear the cache
	buf := make([]*p2p.AveragePricePoint, 0, len(watchSymbols))

	for _, symbol := range watchSymbols {
		avgPricePoint, err := o.priceOracle.GetAveragePrice(symbol)
		if err != nil {
			log.Println("symbol not found in map:", symbol)
			continue
		}
		buf = append(buf, avgPricePoint)
	}

	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgOraclePriceBroadcast,
		Data: buf,
	}
}

func (o *Oracle) relayBtcHeadBlock() {
	headBlock, err := o.btcChainRelayer.FetchChain()
	if err != nil {
		log.Println("failed to fetch BTC head block.", err)
		return
	}

	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgBtcChainRelay,
		Data: headBlock,
	}
}
