package oracle

import (
	"context"
	"fmt"
	"log"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	btcrelay "vsc-node/modules/oracle/btc-relay"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
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

	vStream     *vstream.VStream
	stateEngine *stateEngine.StateEngine

	ctx        context.Context
	cancelFunc context.CancelFunc

	priceOracle *price.PriceOracle

	blockRelay       btcrelay.BtcChainRelay
	blockRelaySignal chan blockTickSignal

	// to be used within a node, for price querying from API endpoints
	observePriceChan chan []p2p.ObservePricePoint

	// to be used within the network, for broadcasting average prices
	broadcastPriceChan   chan []p2p.AveragePricePoint
	broadcastPriceSignal chan blockTickSignal

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
	stateEngine *stateEngine.StateEngine,
) *Oracle {
	return &Oracle{
		p2p:                  p2pServer,
		conf:                 conf,
		electionDb:           electionDb,
		witness:              witness,
		vStream:              vstream,
		stateEngine:          stateEngine,
		blockRelayChan:       make(chan *p2p.BlockRelay, 1),
		blockRelaySignal:     make(chan blockTickSignal, 1),
		msgChan:              make(chan p2p.Msg, 1),
		observePriceChan:     make(chan []p2p.ObservePricePoint, 128),
		broadcastPriceChan:   make(chan []p2p.AveragePricePoint, 128),
		broadcastPriceSignal: make(chan blockTickSignal, 1),
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
	o.vStream.RegisterBlockTick("oracle", o.blockTick, false)
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
