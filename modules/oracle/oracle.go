package oracle

import (
	"context"
	ed25519Std "crypto/ed25519"
	"errors"
	"log"
	"time"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/nonces"
	"vsc-node/modules/db/vsc/witnesses"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	"vsc-node/modules/oracle/chain"
	"vsc-node/modules/oracle/p2p"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

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

	// ~2 minutes = 40 blocks, 3s for every new block.
	// Must span at least 2 witness slots so the first producer's tx confirms
	// before the next producer's oracle tick fires, preventing duplicate submissions.
	chainRelayInterval = uint64(40)
)

var (
	watchSymbols          = []string{"BTC", "ETH", "LTC"}
	errInvalidMessageType = errors.New("invalid message type")

	_ p2p.OracleP2PSpec = &Oracle{}
)

type Oracle struct {
	ctx          context.Context
	cancelFunc   context.CancelFunc
	logger       *vsclog.Logger
	p2pServer    *libp2p.P2PServer
	pubSubSrv    libp2p.PubSubService[p2p.Msg]
	conf         common.IdentityConfig
	oracleConf   OracleConfig
	electionDb   elections.Elections
	witnessDb    witnesses.Witnesses
	hiveConsumer *blockconsumer.HiveConsumer
	stateEngine  *stateEngine.StateEngine
	txPool       *transactionpool.TransactionPool

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
	contractState contracts.ContractState,
	da *DataLayer.DataLayer,
	txPool *transactionpool.TransactionPool,
	oracleConf OracleConfig,
	nonceDb nonces.Nonces,
) *Oracle {
	logger := vsclog.Module("oracle").With("id", conf.Get().HiveUsername)

	ctx, cancel := context.WithCancel(context.Background())

	// txCrafter will be created in Init() after identity config is loaded
	chainRelayer := chain.New(ctx, logger, conf, sconf, electionDb, contractState, da, nil, txPool, nonceDb)

	return &Oracle{
		ctx:          ctx,
		cancelFunc:   cancel,
		p2pServer:    p2pServer,
		conf:         conf,
		oracleConf:   oracleConf,
		electionDb:   electionDb,
		witnessDb:    witnessDb,
		hiveConsumer: hiveConsumer,
		stateEngine:  stateEngine,
		logger:       logger,
		chainOracle:  chainRelayer,
		txPool:       txPool,
	}
}

// Init implements aggregate.Plugin.
// Runs initialization in order of how they are passed in to `Aggregate`
func (o *Oracle) Init() error {
	log.Println("[oracle] Init: registering blockTick callback")
	o.hiveConsumer.RegisterBlockTick("oracle", o.blockTick, true)

	// Configure RPC connections for all chains from oracle config
	cfg := o.oracleConf.Get()
	for symbol := range chain.RegisteredChains() {
		if rpc, ok := cfg.ChainRpc(symbol); ok {
			o.chainOracle.ConfigureChain(symbol, rpc.RpcHost, rpc.RpcUser, rpc.RpcPass)
		}
	}

	// Create the txCrafter now that identity config has been loaded from disk.
	// The libp2p private key is only available after identityConfig.Init().
	if libp2pKey, err := o.conf.Libp2pPrivateKey(); err != nil {
		o.logger.Error("failed to get libp2p private key, chain relay submission disabled", "err", err)
	} else if rawBytes, err := libp2pKey.Raw(); err != nil {
		o.logger.Error("failed to get raw libp2p key bytes, chain relay submission disabled", "err", err)
	} else {
		edPrivKey := ed25519Std.PrivateKey(rawBytes)
		edPubKey := edPrivKey.Public().(ed25519Std.PublicKey)
		if didKey, err := dids.NewKeyDID(edPubKey); err != nil {
			o.logger.Error("failed to create key DID, chain relay submission disabled", "err", err)
		} else {
			txCrafter := &transactionpool.TransactionCrafter{
				Identity: dids.NewKeyProvider(edPrivKey),
				Did:      didKey,
				VSCBroadcast: &transactionpool.InternalBroadcast{
					TxPool: o.txPool,
				},
			}
			o.chainOracle.SetTxCrafter(txCrafter)
			o.logger.Info("chain relay transaction crafter initialized")
		}
	}

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
