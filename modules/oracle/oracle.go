package oracle

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/oracle/chain"
	"vsc-node/modules/oracle/p2p"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"

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

type currentElectionData struct {
	witnesses     []elections.ElectionMember
	blockProducer string
	totalWeight   uint64
	weightMap     []uint64
}

type Oracle struct {
	ctx         context.Context
	logger      *slog.Logger
	p2pServer   *libp2p.P2PServer
	pubSubSrv   libp2p.PubSubService[p2p.Msg]
	conf        common.IdentityConfig
	electionDb  elections.Elections
	witnessDb   witnesses.Witnesses
	vStream     *vstream.VStream
	stateEngine *stateEngine.StateEngine

	// priceOracle *price.PriceOracle
	chainOracle *chain.ChainOracle

	currentElectionData    *currentElectionData
	currentElectionDataMtx *sync.RWMutex
}

func New(
	p2pServer *libp2p.P2PServer,
	conf common.IdentityConfig,
	electionDb elections.Elections,
	witnessDb witnesses.Witnesses,
	vstream *vstream.VStream,
	stateEngine *stateEngine.StateEngine,
) *Oracle {
	// TODO: passsing in context from main thread
	ctx := context.TODO()

	return &Oracle{
		ctx:         ctx,
		p2pServer:   p2pServer,
		conf:        conf,
		electionDb:  electionDb,
		witnessDb:   witnessDb,
		vStream:     vstream,
		stateEngine: stateEngine,
		// priceOracle: priceOracle,
		logger:                 nil,
		chainOracle:            nil,
		currentElectionData:    nil,
		currentElectionDataMtx: &sync.RWMutex{},
	}
}

// Init implements aggregate.Plugin.
// Runs initialization in order of how they are passed in to `Aggregate`
func (o *Oracle) Init() error {
	if os.Getenv("DEBUG") == "1" {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	logger := slog.Default().With("id", o.p2pServer.PeerInfo().GetPeerId())
	// priceOracle := price.New(
	// 	ctx,
	// 	logger,
	// 	userCurrency,
	// 	priceOraclePollInterval,
	// 	watchSymbols,
	// 	conf,
	// )

	o.logger = logger.With("service", "oracle")

	o.chainOracle = chain.New(o.ctx, logger, o.conf)
	subServices := aggregate.New([]aggregate.Plugin{
		o.chainOracle,
		// o.priceOracle,
	})

	return subServices.Init()
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (o *Oracle) Start() *promise.Promise[any] {
	o.vStream.RegisterBlockTick("oracle", o.blockTick, true)

	return promise.New(func(resolve func(any), reject func(error)) {
		o.logger.Debug("starting Oracle service")
		var err error

		o.pubSubSrv, err = libp2p.NewPubSubService(
			o.p2pServer,
			p2p.NewP2pSpec(o),
		)
		if err != nil {
			o.logger.Error("failed to initialize o.service", "err", err)
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
func (o *Oracle) Handle(
	peerID peer.ID,
	msg p2p.Msg,
	// the following 2 arguments are ignored, the interface is meant to be
	// passed down to chainOracle + priceOracle.
	_ string,
	_ []elections.ElectionMember,
) (p2p.Msg, error) {
	o.currentElectionDataMtx.RLock()
	defer o.currentElectionDataMtx.RUnlock()

	if o.currentElectionData == nil {
		return nil, errors.New("invalid current election data")
	}

	var (
		handler p2p.MessageHandler

		blockProducer = o.currentElectionData.blockProducer
		witnesses     = o.currentElectionData.witnesses
	)

	switch msg.Code {
	// case p2p.MsgPriceBroadcast, p2p.MsgPriceSignature, p2p.MsgPriceBlock:
	// 	handler = o.priceOracle

	case p2p.MsgChainRelay:
		handler = o.chainOracle

	default:
		return nil, errInvalidMessageType
	}

	return handler.Handle(peerID, msg, blockProducer, witnesses)
}
