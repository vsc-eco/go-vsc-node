package oracle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
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
	watchSymbols          = []string{"BTC", "ETH", "LTC"}
	errInvalidMessageType = errors.New("invalid message type")
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

	priceOracle *price.PriceOracle

	// unlocks during block ticks interval
	broadcastPricePoints *threadsafe.Map[string, []pricePoint]
	broadcastPriceSig    *threadsafe.Slice[p2p.OracleBlock]
	broadcastPriceBlocks *threadsafe.Slice[p2p.OracleBlock]

	chainOracle *chainrelay.ChainRelayer
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
	}
}

// Init implements aggregate.Plugin.
// Runs initialization in order of how they are passed in to `Aggregate`
func (o *Oracle) Init() error {
	o.ctx, o.cancelFunc = context.WithCancel(context.Background())

	var err error

	o.priceOracle, err = price.New(o.logger, userCurrency)
	if err != nil {
		return fmt.Errorf("failed to initialize price oracle: %w", err)
	}

	o.chainOracle, err = chainrelay.New(o.logger)
	if err != nil {
		return fmt.Errorf("failed to initialize chain relay oracle: %w", err)
	}

	// locking states
	o.broadcastPricePoints.Lock()
	o.broadcastPriceSig.Lock()
	o.broadcastPriceBlocks.Lock()

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
		return o.handlePriceMsg(peerID, msg)

	case p2p.MsgChainRelayBlock:
		return o.handleChainRelayMsg(peerID, msg)

	default:
		return nil, errInvalidMessageType
	}
}

func (o *Oracle) handlePriceMsg(
	peerID peer.ID,
	msg p2p.Msg,
) (p2p.Msg, error) {

	var response p2p.Msg
	switch msg.Code {
	case p2p.MsgPriceBroadcast:
		data, err := parseRawJson[map[string]p2p.AveragePricePoint](msg.Data)
		if err != nil {
			return nil, err
		}

		pricePointCollector := collectPricePoint(peerID, data)
		if !o.broadcastPricePoints.Update(pricePointCollector) {
			o.logger.Debug(
				"unable to collect broadcasted average price points in the current block interval.",
			)
		}

	case p2p.MsgPriceSignature:
		block, err := parseRawJson[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		block.TimeStamp = time.Now().UTC()

		if !o.broadcastPriceSig.Append(*block) {
			o.logger.Debug(
				"unable to collect signature in the current block interval.",
			)
		}

	case p2p.MsgPriceBlock:
		block, err := parseRawJson[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		block.TimeStamp = time.Now().UTC()
		if !o.broadcastPriceBlocks.Append(*block) {
			o.logger.Debug(
				"unable to collect and verify price block in the current block interval.",
			)
		}

	default:
		return nil, errInvalidMessageType
	}

	return response, nil
}

func collectPricePoint(
	peerID peer.ID,
	data *map[string]p2p.AveragePricePoint,
) threadsafe.MapUpdateFunc[string, []pricePoint] {
	return func(m map[string][]pricePoint) {
		recvTimeStamp := time.Now().UTC()

		for symbol, avgPricePoint := range *data {
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

func (o *Oracle) handleChainRelayMsg(
	peerID peer.ID,
	msg p2p.Msg,
) (p2p.Msg, error) {
	const threadBlocking = false
	var response p2p.Msg = nil

	switch msg.Code {
	case p2p.MsgChainRelayBlock:
		block, err := parseRawJson[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		if !o.chainOracle.SignBlockBuf.Append(*block) {
			o.logger.Debug(
				"unable to collect and verify price block in the current block interval.",
			)
		}

	default:
		return nil, errInvalidMessageType
	}

	return response, nil
}
