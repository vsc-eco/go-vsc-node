package oracle

import (
	"context"
	"fmt"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"

	"github.com/chebyrash/promise"
)

const (
	// 10 minutes = 600 seconds, 3s for every new block.
	// A tick is broadcasted every 200 blocks produced.
	priceOracleBroadcastInterval = uint64(600 / 3)
	priceOraclePollInterval      = time.Second * 15

	// 10 minutes = 600 seconds, 3s for every new block
	btcChainRelayInterval = uint64(600 / 3)
)

var (
	watchSymbols = []string{"BTC", "ETH", "LTC"}
)

type BlockSync interface {
	RegisterBlockTick(string, vstream.BTFunc, bool)
}

type BlockSchedule interface {
	GetSchedule(slotHeight uint64) []stateEngine.WitnessSlot
}

type Oracle struct {
	p2pServer  *libp2p.P2PServer
	oracleP2P  p2p.OracleP2pParams
	service    libp2p.PubSubService[p2p.Msg]
	conf       common.IdentityConfig
	electionDb elections.Elections
	witness    witnesses.Witnesses

	vStream     BlockSync
	stateEngine BlockSchedule

	ctx        context.Context
	cancelFunc context.CancelFunc

	priceOracle          *price.PriceOracle
	broadcastPriceSignal chan blockTickSignal

	// to be used within the network, for broadcasting average prices
	broadcastPriceChan chan []p2p.AveragePricePoint

	// for block signatures
	priceBlockSignatureChan chan p2p.VSCBlock

	// for new block
	broadcastPriceBlockChan chan p2p.VSCBlock

	// for communication between nodes in a network
	msgChan chan p2p.Msg
}

func New(
	p2pServer *libp2p.P2PServer,
	oracleP2P p2p.OracleP2pParams,
	conf common.IdentityConfig,
	electionDb elections.Elections,
	witness witnesses.Witnesses,
	vstream BlockSync,
	stateEngine BlockSchedule,
) *Oracle {
	return &Oracle{
		p2pServer:   p2pServer,
		oracleP2P:   oracleP2P,
		conf:        conf,
		electionDb:  electionDb,
		witness:     witness,
		vStream:     vstream,
		stateEngine: stateEngine,

		msgChan: make(chan p2p.Msg, 1),

		broadcastPriceChan:      make(chan []p2p.AveragePricePoint, 128),
		broadcastPriceSignal:    make(chan blockTickSignal, 1),
		priceBlockSignatureChan: make(chan p2p.VSCBlock, 1024),
		broadcastPriceBlockChan: make(chan p2p.VSCBlock, 1024),
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

	// o.blockRelay = btcrelay.New(o.blockRelayChan)

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

		o.oracleP2P.Initialize(
			o.broadcastPriceChan,
			o.priceBlockSignatureChan,
			o.broadcastPriceBlockChan,
		)
		o.service, err = libp2p.NewPubSubService(o.p2pServer, o.oracleP2P)
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

func (o *Oracle) relayBtcHeadBlock() {
	/*
		headBlock, err := o.blockRelay.FetchChain()
		if err != nil {
			log.Println("failed to fetch BTC head block.", err)
			return
		}

		o.msgChan <- &p2p.OracleMessage{
			Type: p2p.MsgBtcChainRelay,
			Data: headBlock,
		}
	*/
}
