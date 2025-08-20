package oracle

import (
	"context"
	"encoding/json"
	"time"
	"vsc-node/modules/common"
	btcrelay "vsc-node/modules/oracle/btc-relay"
	"vsc-node/modules/oracle/price"
	libp2p "vsc-node/modules/p2p"

	"github.com/chebyrash/promise"
)

type MsgType string

const (
	topic = "/vsc/mainet/oracle/v1"

	btcChainRelayInterval = time.Minute * 10
	btcChainRelayMsgType  = MsgType("btc-chain-relay")

	priceOracleUpdateInterval = time.Hour
	priceOracleMsgType        = MsgType("price-orcale")
)

type (
	Msg    *oracleMessage
	Oracle struct {
		p2p     *libp2p.P2PServer
		service libp2p.PubSubService[Msg]
		conf    common.IdentityConfig

		ctx        context.Context
		cancelFunc context.CancelFunc

		priceOracle     price.PriceOracle
		btcChainRelayer btcrelay.BtcChainRelay
	}

	oracleMessage struct {
		Type MsgType `validate:"required"`
		Data json.Unmarshaler
	}
)

func New(p2pServer *libp2p.P2PServer, conf common.IdentityConfig) *Oracle {
	ctx, cancel := context.WithCancel(context.Background())
	return &Oracle{
		p2p:     p2pServer,
		service: nil,
		conf:    conf,

		ctx:        ctx,
		cancelFunc: cancel,

		priceOracle: price.New(),

		btcChainRelayer: btcrelay.New(),
	}
}

// Init implements aggregate.Plugin.
// Runs initialization in order of how they are passed in to `Aggregate`
func (o *Oracle) Init() error {
	return nil
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (o *Oracle) Start() *promise.Promise[any] {
	go o.btcChainRelayer.Poll(o.ctx, btcChainRelayInterval)
	go o.priceOracle.Poll(o.ctx, priceOracleUpdateInterval)

	return promise.New(func(resolve func(any), reject func(error)) {
		var err error

		o.service, err = libp2p.NewPubSubService(o.p2p, &p2pSpec{
			conf:   o.conf,
			oracle: o,
		})
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
