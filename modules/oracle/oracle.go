package oracle

import (
	"context"
	"fmt"
	"log"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	btcrelay "vsc-node/modules/oracle/btc-relay"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price"
	libp2p "vsc-node/modules/p2p"

	"github.com/chebyrash/promise"
)

const (
	btcChainRelayInterval        = time.Minute * 10
	priceOracleBroadcastInterval = time.Hour
)

type Oracle struct {
	p2p        *libp2p.P2PServer
	service    libp2p.PubSubService[p2p.Msg]
	conf       common.IdentityConfig
	electionDb elections.Elections
	ctx        context.Context
	cancelFunc context.CancelFunc

	priceOracle *price.PriceOracle

	btcChainRelayer btcrelay.BtcChainRelay
	btcChan         chan *btcrelay.BtcHeadBlock

	msgChan chan p2p.Msg
}

func New(
	p2pServer *libp2p.P2PServer,
	conf common.IdentityConfig,
	electionDb elections.Elections,
) *Oracle {
	return &Oracle{
		p2p:        p2pServer,
		conf:       conf,
		electionDb: electionDb,
		btcChan:    make(chan *btcrelay.BtcHeadBlock),
		msgChan:    make(chan p2p.Msg),
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
	go o.observe()

	return promise.New(func(resolve func(any), reject func(error)) {
		var (
			err    error
			p2pSrv = p2p.New(o.conf)
		)

		o.service, err = libp2p.NewPubSubService(o.p2p, p2pSrv)
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

func (o *Oracle) observe() {
	go o.priceOracle.Poll(o.ctx, priceOracleBroadcastInterval, o.msgChan)
	go o.btcChainRelayer.Poll(o.ctx, btcChainRelayInterval, o.msgChan)

	for {
		select {
		case <-o.ctx.Done():
			return

		case msg := <-o.msgChan:
			if err := o.service.Send(msg); err != nil {
				log.Println("[oracle] failed broadcast message", msg, err)
			}
		}
	}
}
