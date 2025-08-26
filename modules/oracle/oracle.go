package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

	priceOracleBroadcastInterval = time.Hour
	priceOracleMsgType           = MsgType("price-orcale")
)

type Oracle struct {
	p2p     *libp2p.P2PServer
	service libp2p.PubSubService[Msg]
	conf    common.IdentityConfig

	ctx        context.Context
	cancelFunc context.CancelFunc

	priceOracle     *price.PriceOracle
	btcChainRelayer btcrelay.BtcChainRelay
}

type Msg *oracleMessage

type oracleMessage struct {
	Type MsgType        `json:"type,omitempty" validate:"required"`
	Data json.Marshaler `json:"data,omitempty" validate:"required"`
}

func New(
	p2pServer *libp2p.P2PServer,
	conf common.IdentityConfig,
) *Oracle {
	return &Oracle{
		p2p:  p2pServer,
		conf: conf,
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

	o.btcChainRelayer = btcrelay.New()

	o.ctx, o.cancelFunc = context.WithCancel(context.Background())

	return nil
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (o *Oracle) Start() *promise.Promise[any] {
	go o.btcChainRelayer.Poll(o.ctx, btcChainRelayInterval)

	go o.marketObserve()

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

func (o *Oracle) marketObserve() {
	pricePointChan := make(chan []*price.AveragePricePoint, 10)
	go o.priceOracle.Poll(o.ctx, priceOracleBroadcastInterval, pricePointChan)

	for {
		select {
		case <-o.ctx.Done():
			return

		case pricePoints := <-pricePointChan:
			msg := oracleMessage{
				Type: priceOracleMsgType,
				Data: &jsonSerializer[[]*price.AveragePricePoint]{pricePoints},
			}
			if err := o.service.Send(&msg); err != nil {
				log.Println("[oracle] failed to send price points", err)
			}
		}
	}
}

type jsonSerializer[T any] struct {
	data T
}

func (js *jsonSerializer[any]) MarshalJSON() ([]byte, error) {
	return json.Marshal(js.data)
}
