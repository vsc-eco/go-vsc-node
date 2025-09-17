package oracle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	chainrelay "vsc-node/modules/oracle/chain-relay"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price"
	"vsc-node/modules/oracle/threadsafe"
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
	chainRelayInterval = uint64(600 / 3)
)

var (
	watchSymbols = []string{"BTC", "ETH", "LTC"}
)

type Oracle struct {
	p2pServer   *libp2p.P2PServer
	service     libp2p.PubSubService[p2p.Msg]
	conf        common.IdentityConfig
	electionDb  elections.Elections
	witness     witnesses.Witnesses
	vStream     *vstream.VStream
	stateEngine *stateEngine.StateEngine

	logger *slog.Logger

	ctx        context.Context
	cancelFunc context.CancelFunc

	priceOracle          *price.PriceOracle
	broadcastPricePoints *threadsafe.Map[string, []pricePoint]
	broadcastPriceSig    *threadsafe.Slice[p2p.OracleBlock]
	broadcastPriceBlocks *threadsafe.Slice[p2p.OracleBlock]
	broadcastPriceFlags  broadcastPriceFlags

	chainOracle *chainrelay.ChainRelayer
}

type broadcastPriceFlags struct {
	isBroadcastTickInterval  bool
	isCollectingAveragePrice bool
	isCollectingSignatures   bool
	lock                     *sync.RWMutex
}

func New(
	p2pServer *libp2p.P2PServer,
	conf common.IdentityConfig,
	electionDb elections.Elections,
	witness witnesses.Witnesses,
	vstream *vstream.VStream,
	stateEngine *stateEngine.StateEngine,
) *Oracle {
	logLevel := slog.LevelInfo
	if os.Getenv("DEBUG") == "1" {
		logLevel = slog.LevelDebug
	}

	logHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})

	logger := slog.New(logHandler).With("service", "oracle")

	return &Oracle{
		p2pServer:   p2pServer,
		conf:        conf,
		electionDb:  electionDb,
		witness:     witness,
		vStream:     vstream,
		stateEngine: stateEngine,
		logger:      logger,

		broadcastPricePoints: threadsafe.NewMap[string, []pricePoint](),
		broadcastPriceSig:    threadsafe.NewSlice[p2p.OracleBlock](512),
		broadcastPriceBlocks: threadsafe.NewSlice[p2p.OracleBlock](512),
		broadcastPriceFlags: broadcastPriceFlags{
			isBroadcastTickInterval:  false,
			isCollectingAveragePrice: false,
			isCollectingSignatures:   false,
			lock:                     new(sync.RWMutex),
		},
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

	o.chainOracle, err = chainrelay.New(o.logger)
	if err != nil {
		return fmt.Errorf("failed to initialize chain relay oracle: %w", err)
	}

	return nil
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (o *Oracle) Start() *promise.Promise[any] {
	o.vStream.RegisterBlockTick("oracle", o.blockTick, true)

	return promise.New(func(resolve func(any), reject func(error)) {
		o.logger.Debug("starting Oracle service")
		var err error

		o.service, err = libp2p.NewPubSubService(o.p2pServer, p2p.NewP2pSpec(o))
		// panic("alskjdflkj")
		if err != nil {
			o.logger.Error("failed to initialize o.service", "err", err)
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

func (o *Oracle) BroadcastMessage(msgCode p2p.MsgCode, data any) error {
	if o.service == nil {
		return errors.New("o.service uninitialized")
	}

	msg, err := p2p.MakeOracleMessage(msgCode, data)
	if err != nil {
		return err
	}

	return o.service.Send(msg)
}

// Handle implements p2p.MessageHandler
func (o *Oracle) Handle(peerID peer.ID, msg p2p.Msg) (p2p.Msg, error) {
	switch msg.Code {
	case p2p.MsgPriceBroadcast, p2p.MsgPriceSignature, p2p.MsgPriceBlock:
		o.broadcastPriceFlags.lock.RLock()
		defer o.broadcastPriceFlags.lock.RUnlock()

		return o.handlePriceMsg(peerID, msg, o.broadcastPriceFlags)

	default:
		return nil, errors.New("invalid message type")
	}
}

func (o *Oracle) handlePriceMsg(
	peerID peer.ID,
	msg p2p.Msg,
	oracleState broadcastPriceFlags,
) (p2p.Msg, error) {
	var response p2p.Msg

	switch msg.Code {
	case p2p.MsgPriceBroadcast:
		if !oracleState.isCollectingAveragePrice {
			return nil, nil
		}

		data, err := parseMsg[map[string]p2p.AveragePricePoint](msg.Data)
		if err != nil {
			return nil, err
		}

		o.broadcastPricePoints.Update(collectPricePoint(peerID, *data))

	case p2p.MsgPriceSignature:
		if !oracleState.isCollectingSignatures {
			return nil, nil
		}

		block, err := parseMsg[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		block.TimeStamp = time.Now().UTC()
		o.broadcastPriceSig.Append(*block)

	case p2p.MsgPriceBlock:
		if !oracleState.isBroadcastTickInterval {
			return nil, nil
		}

		block, err := parseMsg[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		block.TimeStamp = time.Now().UTC()
		o.broadcastPriceBlocks.Append(*block)
	}

	return response, nil
}

func collectPricePoint(
	peerID peer.ID,
	data map[string]p2p.AveragePricePoint,
) threadsafe.MapUpdateFunc[string, []pricePoint] {
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
