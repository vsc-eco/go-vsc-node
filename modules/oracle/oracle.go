package oracle

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// NOTE: only supporting USD for now
	userCurrency = "usd"

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

type Oracle struct {
	p2pServer  *libp2p.P2PServer
	service    libp2p.PubSubService[p2p.Msg]
	conf       common.IdentityConfig
	electionDb elections.Elections
	witness    witnesses.Witnesses

	vStream     *vstream.VStream
	stateEngine *stateEngine.StateEngine

	ctx        context.Context
	cancelFunc context.CancelFunc

	priceOracle *price.PriceOracle

	// to be used within the network, for broadcasting average prices
	broadcastPricePoints *threadSafeMap[string, []pricePoint]
	broadcastPriceTick   chan blockTickSignal

	// for block signatures
	priceBlockSignatureChan chan p2p.VSCBlock

	// for new block
	broadcastPriceBlockChan chan p2p.VSCBlock
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
		p2pServer:   p2pServer,
		conf:        conf,
		electionDb:  electionDb,
		witness:     witness,
		vStream:     vstream,
		stateEngine: stateEngine,

		broadcastPricePoints: makeThreadSafeMap[string, []pricePoint](),
		broadcastPriceSignal: make(chan blockTickSignal, 1),

		priceBlockSignatureChan: make(chan p2p.VSCBlock, 1024),
		broadcastPriceBlockChan: make(chan p2p.VSCBlock, 1024),
	}
}

// Init implements aggregate.Plugin.
// Runs initialization in order of how they are passed in to `Aggregate`
func (o *Oracle) Init() error {
	o.ctx, o.cancelFunc = context.WithCancel(context.Background())

	var err error

	o.priceOracle, err = price.New(userCurrency)
	if err != nil {
		return fmt.Errorf("failed to initialize price oracle: %w", err)
	}

	// o.blockRelay = btcrelay.New(o.blockRelayChan)

	return nil
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (o *Oracle) Start() *promise.Promise[any] {
	o.vStream.RegisterBlockTick("oracle", o.blockTick, true)

	return promise.New(func(resolve func(any), reject func(error)) {
		var err error

		o.service, err = libp2p.NewPubSubService(o.p2pServer, p2p.NewP2pSpec(o))
		if err != nil {
			o.cancelFunc()
			reject(err)
			return
		}

		go o.marketObserve()
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

func (o *Oracle) BroadcastMessage(msg p2p.Msg) error {
	if o.service == nil {
		return errors.New("o.service uninitialized")
	}

	if msg == nil {
		return nil
	}

	return o.service.Send(msg)
}

// Handle implements p2p.MessageHandler
func (o *Oracle) Handle(peerID peer.ID, msg p2p.Msg) (p2p.Msg, error) {
	var responseMsg p2p.Msg = nil

	switch msg.Type {
	case p2p.MsgPriceBroadcast:
		data, err := parseMsg[map[string]p2p.AveragePricePoint](msg.Data)
		if err != nil {
			return nil, err
		}

		o.broadcastPricePoints.Update(collectPricePoint(peerID, *data))

	default:
		return nil, errors.New("invalid message type")
	}

	return responseMsg, nil
}

func collectPricePoint(
	peerID peer.ID,
	data map[string]p2p.AveragePricePoint,
) updateFunc[string, []pricePoint] {
	return func(m map[string][]pricePoint) {
		recvTimeStamp := time.Now().UTC()

		for symbol, avgPricePoint := range data {
			sym := strings.ToUpper(symbol)

			pp := pricePoint{
				price:       avgPricePoint.Price,
				volume:      avgPricePoint.Volume,
				peerID:      peerID,
				collectedAt: recvTimeStamp,
			}

			v, ok := m[sym]
			if !ok {
				v = []pricePoint{pp}
			} else {
				v = append(v, pp)
			}

			m[sym] = v
		}
	}
}
