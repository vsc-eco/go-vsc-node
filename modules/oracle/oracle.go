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
	"vsc-node/modules/vstream"

	"github.com/chebyrash/promise"
)

const (
	// 10 minutes = 600 seconds, 3s for every new block
	priceOracleBroadcastInterval = uint64(600 / 3)
	priceOraclePollInterval      = time.Second * 15

	// 10 minutes = 600 seconds, 3s for every new block
	btcChainRelayInterval = uint64(600 / 3)
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
	VStream    *vstream.VStream

	ctx        context.Context
	cancelFunc context.CancelFunc

	priceOracle *price.PriceOracle

	blockRelay       btcrelay.BtcChainRelay
	blockRelaySignal chan struct{}

	// to be used within a node, for price querying from API endpoints
	observePriceChan chan []p2p.ObservePricePoint

	// to be used within the network, for broadcasting average prices
	broadcastPriceChan   chan []p2p.AveragePricePoint
	broadcastPriceSignal chan struct{}

	blockRelayChan chan *p2p.BlockRelay

	// for communication between nodes in a network
	msgChan chan p2p.Msg
}

func New(
	p2pServer *libp2p.P2PServer,
	conf common.IdentityConfig,
	electionDb elections.Elections,
	witness witnesses.Witnesses,
	vstream *vstream.VStream,
) *Oracle {
	return &Oracle{
		p2p:                  p2pServer,
		conf:                 conf,
		electionDb:           electionDb,
		witness:              witness,
		VStream:              vstream,
		blockRelayChan:       make(chan *p2p.BlockRelay),
		blockRelaySignal:     make(chan struct{}, 1),
		msgChan:              make(chan p2p.Msg),
		observePriceChan:     make(chan []p2p.ObservePricePoint, 10),
		broadcastPriceChan:   make(chan []p2p.AveragePricePoint, 100),
		broadcastPriceSignal: make(chan struct{}, 1),
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

	o.blockRelay = btcrelay.New(o.blockRelayChan)

	o.ctx, o.cancelFunc = context.WithCancel(context.Background())

	return nil
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (o *Oracle) Start() *promise.Promise[any] {
	o.VStream.RegisterBlockTick("oracle", o.BlockTick, false)
	go o.marketObserve()

	return promise.New(func(resolve func(any), reject func(error)) {
		var err error

		o.service, err = libp2p.NewPubSubService(
			o.p2p,
			p2p.New(o.conf, o.blockRelayChan, o.broadcastPriceChan),
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
		pricePollTicker = time.NewTicker(priceOraclePollInterval)
	)

	for {
		select {
		case <-o.ctx.Done():
			return

		case <-pricePollTicker.C:
			for _, api := range o.priceOracle.PriceAPIs {
				go api.QueryMarketPrice(watchSymbols, o.observePriceChan)
			}

		case <-o.broadcastPriceSignal:
			o.broadcastPricePoints()
			isBlockProducer := true
			if isBlockProducer {
				vscBlock, err := o.broadcastMedianPrice()
				if err != nil {
					log.Println(err)
					continue
				}
				o.pollMedianPriceSignature(vscBlock)
			}

		case <-o.blockRelaySignal:
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

		case btcHeadBlock := <-o.blockRelayChan:
			fmt.Println("TODO: validate btcHeadBlock", btcHeadBlock)

		}
	}
}

func (o *Oracle) pollMedianPriceSignature(block *p2p.VSCBlock) {
	/*
		// collect signatures
		// TODO: how do i get the latest witnesses?
		witnesses, err := o.witness.GetLastestWitnesses()
		if err != nil {
			log.Println("failed to get latest witnesses", err)
			return
		}
		log.Println(witnesses)
	*/

	// TODO: validate 2/3 of witness signatures
	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgPriceOracleSignedBlock,
		Data: *block,
	}
}

// returns nil if the node is not the block producer
func (o *Oracle) broadcastPricePoints() {
	defer o.priceOracle.ResetPriceCache()

	// local price

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
			return nil
		}
	*/

}

type pricePoints struct {
	prices  []float64
	volumes []float64
}

type medianPricePointMap = map[string]pricePoints

func (o *Oracle) broadcastMedianPrice() (*p2p.VSCBlock, error) {
	medPriceMap := make(medianPricePointMap)

	// listen on this channel for 10 seconds, room for network latency
	observeAvgPriceCtx, observeAvgPriceCtxCancel := context.WithTimeout(
		context.Background(),
		time.Second*10,
	)
	defer observeAvgPriceCtxCancel()

observeAvgPrice:
	for {
		select {
		case <-observeAvgPriceCtx.Done():
			break observeAvgPrice

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
		return nil, err
	}

	// broadcast new unsigned block
	block := p2p.VSCBlock{
		Signatures: []string{},
		Data:       string(jsonData),
	}

	msg := &p2p.OracleMessage{
		Type: p2p.MsgPriceOracleNewBlock,
		Data: block,
	}

	o.msgChan <- msg

	return &block, nil
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
	headBlock, err := o.blockRelay.FetchChain()
	if err != nil {
		log.Println("failed to fetch BTC head block.", err)
		return
	}

	o.msgChan <- &p2p.OracleMessage{
		Type: p2p.MsgBtcChainRelay,
		Data: headBlock,
	}
}

func (o *Oracle) BlockTick(bh uint64, headHeight *uint64) {
	if headHeight == nil {
		return
	}

	if *headHeight%priceOracleBroadcastInterval == 0 {
		o.broadcastPriceSignal <- struct{}{}
	}
	if *headHeight%btcChainRelayInterval == 0 {
		o.blockRelaySignal <- struct{}{}
	}
}
