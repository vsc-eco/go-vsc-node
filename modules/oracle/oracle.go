package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	btcrelay "vsc-node/modules/oracle/btc-relay"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price"
	libp2p "vsc-node/modules/p2p"

	"github.com/chebyrash/promise"
)

const (
	btcChainRelayInterval        = time.Minute * 10
	priceOracleBroadcastInterval = time.Minute * 10
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
	witness    witnesses.Witnesses

	ctx        context.Context
	cancelFunc context.CancelFunc

	priceOracle *price.PriceOracle

	btcChainRelayer btcrelay.BtcChainRelay

	// to be used within a node, for price querying from API endpoints
	observePriceChan chan []p2p.ObservePricePoint

	// to be used within the network, for broadcasting average prices
	broadcastPriceChan chan []p2p.AveragePricePoint

	btcChan chan *p2p.BtcHeadBlock

	// for communication between nodes in a network
	msgChan chan p2p.Msg
}

func New(
	p2pServer *libp2p.P2PServer,
	conf common.IdentityConfig,
	electionDb elections.Elections,
	witness witnesses.Witnesses,
) *Oracle {
	return &Oracle{
		p2p:                p2pServer,
		conf:               conf,
		electionDb:         electionDb,
		witness:            witness,
		btcChan:            make(chan *p2p.BtcHeadBlock),
		msgChan:            make(chan p2p.Msg),
		observePriceChan:   make(chan []p2p.ObservePricePoint, 10),
		broadcastPriceChan: make(chan []p2p.AveragePricePoint, 100),
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
			p2p.New(o.conf, o.btcChan, o.broadcastPriceChan),
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
		pricePollTicker     = time.NewTicker(priceOraclePollInterval)
		btcChainRelayTicker = time.NewTicker(btcChainRelayInterval)

		// need to use blokci nterval
		priceBroadcastTicker = time.NewTicker(priceOracleBroadcastInterval)
	)

	for {
		select {
		case <-o.ctx.Done():
			return

		case <-pricePollTicker.C:
			for _, api := range o.priceOracle.PriceAPIs {
				go api.QueryMarketPrice(watchSymbols, o.observePriceChan)
			}

		case <-priceBroadcastTicker.C:
			//@TODO Use block tick interval
			o.broadcastPricePoints()

		case <-btcChainRelayTicker.C:
			//@TODO: Use block ticket interval as well for 10 minute intervals
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
	defer o.priceOracle.ResetPriceCache()

	blockChan := make(chan *p2p.VSCBlock, 1)
	defer close(blockChan)

	go o.pollAveragePrice(blockChan)

	// local price

	// TODO: if this node is do an early continue and clear the cache
	buf := make([]p2p.AveragePricePoint, 0, len(watchSymbols))

	for _, symbol := range watchSymbols {
		avgPricePoint, err := o.priceOracle.GetAveragePrice(symbol)
		if err != nil {
			log.Println("symbol not found in map:", symbol)
			continue
		}
		buf = append(buf, *avgPricePoint)
	}

	o.broadcastPriceChan <- buf

	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgPriceOracleBroadcast,
		Data: buf,
	}

	/*
		if node is not a block producer {
			- calculate the median of each symbols
			- compare with the broadcasted block
			- sign and broadcast the signature
			return
		}
	*/

	// broadcast new block
	// TODO: how to determine if this node is a witness
	block := <-blockChan

	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgPriceOracleNewBlock,
		Data: block,
	}

	// collect signatures
	// TODO: how do i get the latest witnesses?
	witnesses, err := o.witness.GetLastestWitnesses()
	if err != nil {
		log.Println("failed to get latest witnesses", err)
		return
	}
	log.Println(witnesses)
}

type pricePoints struct {
	prices  []float64
	volumes []float64
}

type medianPricePointMap = map[string]pricePoints

func (o *Oracle) pollAveragePrice(
	vscBlockChan chan<- *p2p.VSCBlock,
) {
	medPriceMap := make(medianPricePointMap)

	// listen on this channel for 10 seconds, room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			break

		case avgPriceBroadcasted := <-o.broadcastPriceChan:
			for _, p := range avgPriceBroadcasted {
				/*
					if out of time frame {
						continue
					}
				*/
				symbol := strings.ToUpper(p.Symbol)

				medPrice, ok := medPriceMap[symbol]
				if !ok {
					medPrice = pricePoints{[]float64{}, []float64{}}
				}
				medPrice.prices = append(medPrice.prices, p.Price)
				medPrice.volumes = append(medPrice.prices, p.Volume)

				medPriceMap[symbol] = medPrice
			}
		}
	}

	//
	medianPricePoint := make([]p2p.AveragePricePoint, 0, 16)
	for symbol, pricePoints := range medPriceMap {
		m := p2p.AveragePricePoint{
			Symbol: symbol,
			Price:  getMedian(pricePoints.prices),
			Volume: getMedian(pricePoints.volumes),
		}

		medianPricePoint = append(medianPricePoint, m)
	}

	jsonData, err := json.Marshal(medianPricePoint)
	if err != nil {
		log.Println(err)
		return
	}

	vscBlockChan <- &p2p.VSCBlock{
		Signatures: []string{},
		Data:       string(jsonData),
	}
}

func getMedian(buf []float64) float64 {
	slices.Sort(buf)

	evenCount := len(buf)&1 == 0
	if evenCount {
		i := len(buf) / 2
		return (buf[i] + buf[i-1]) / 2
	} else {
		return buf[len(buf)/2]
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
