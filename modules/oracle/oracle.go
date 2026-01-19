package oracle

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	"vsc-node/modules/oracle/chain"
	"vsc-node/modules/oracle/p2p"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/chebyrash/promise"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// NOTE: only supporting USD for now
	userCurrency = "usd"

	// 10 minutes = 600 seconds or 200 blocks, 3s for every new block.
	// A tick is broadcasted every 200 blocks produced.
	priceOracleBroadcastInterval = uint64(600 / 3)
	priceOraclePollInterval      = time.Second * 15

	// 10 minutes = 600 seconds or 200 blocks, 3s for every new block
	chainRelayInterval = uint64(600 / 3)
)

var (
	watchSymbols          = []string{"BTC", "ETH", "LTC"}
	errInvalidMessageType = errors.New("invalid message type")

	_ p2p.OracleP2PSpec = &Oracle{}
)

type Oracle struct {
	ctx          context.Context
	cancelFunc   context.CancelFunc
	logger       *slog.Logger
	p2pServer    *libp2p.P2PServer
	pubSubSrv    libp2p.PubSubService[p2p.Msg]
	conf         common.IdentityConfig
	electionDb   elections.Elections
	witnessDb    witnesses.Witnesses
	hiveConsumer *blockconsumer.HiveConsumer
	stateEngine  *stateEngine.StateEngine

	// priceOracle *price.PriceOracle
	chainOracle *chain.ChainOracle
}

func New(
	p2pServer *libp2p.P2PServer,
	conf common.IdentityConfig,
	sconf systemconfig.SystemConfig,
	electionDb elections.Elections,
	witnessDb witnesses.Witnesses,
	hiveConsumer *blockconsumer.HiveConsumer,
	stateEngine *stateEngine.StateEngine,
) *Oracle {
	logLevel := slog.LevelInfo
	if os.Getenv("DEBUG") == "1" {
		logLevel = slog.LevelDebug
	}

	logHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})

	logger := slog.New(logHandler).
		With("service", "oracle").
		With("id", conf.Get().HiveUsername)

	ctx, cancel := context.WithCancel(context.Background())

	// priceOracle := price.New(
	// 	ctx,
	// 	logger,
	// 	userCurrency,
	// 	priceOraclePollInterval,
	// 	watchSymbols,
	// 	conf,
	// )

	chainRelayer := chain.New(ctx, logger, conf, sconf)

	return &Oracle{
		ctx:          ctx,
		cancelFunc:   cancel,
		p2pServer:    p2pServer,
		conf:         conf,
		electionDb:   electionDb,
		witnessDb:    witnessDb,
		hiveConsumer: hiveConsumer,
		stateEngine:  stateEngine,
		logger:       logger,
		// priceOracle: priceOracle,
		chainOracle: chainRelayer,
	}
}

// Init implements aggregate.Plugin.
// Runs initialization in order of how they are passed in to `Aggregate`
func (o *Oracle) Init() error {
	services := []aggregate.Plugin{
		o.chainOracle,
		// o.priceOracle,
	}
	return aggregate.New(services).Init()
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (o *Oracle) Start() *promise.Promise[any] {
	// o.vStream.RegisterBlockTick("oracle", o.blockTick, true)

	return promise.New(func(resolve func(any), reject func(error)) {
		o.logger.Debug("starting Oracle service")
		var err error

		o.pubSubSrv, err = libp2p.NewPubSubService(
			o.p2pServer,
			p2p.NewP2pSpec(o),
		)
		if err != nil {
			o.logger.Error("failed to initialize o.service", "err", err)
			o.cancelFunc()
			reject(err)
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		services := []aggregate.Plugin{
			o.chainOracle,
			// o.priceOracle,
		}

		s := aggregate.New(services)
		if _, err := s.Start().Await(ctx); err != nil {
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

	services := []aggregate.Plugin{
		o.chainOracle,
		// o.priceOracle,
	}

	s := aggregate.New(services)
	if err := s.Stop(); err != nil {
		return err
	}

	if o.pubSubSrv == nil {
		return nil
	}

	return o.pubSubSrv.Close()
}

// Broadcast implements p2p.OracleVscSpec.
func (o *Oracle) Broadcast(msgCode p2p.MsgCode, data any) error {
	if o.pubSubSrv == nil {
		return errors.New("o.service uninitialized")
	}

	msg, err := p2p.MakeOracleMessage(msgCode, data)
	if err != nil {
		return err
	}

	return o.pubSubSrv.Send(msg)
}

// Handle implements p2p.MessageHandler
func (o *Oracle) Handle(peerID peer.ID, msg p2p.Msg) (p2p.Msg, error) {
	var handler p2p.MessageHandler

	switch msg.Code {
	// case p2p.MsgPriceBroadcast, p2p.MsgPriceSignature, p2p.MsgPriceBlock:
	// 	handler = o.priceOracle

	case p2p.MsgChainRelay:
		handler = o.chainOracle

	default:
		return nil, errInvalidMessageType
	}

	return handler.Handle(peerID, msg)
}
