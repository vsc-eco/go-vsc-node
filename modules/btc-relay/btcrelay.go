package btcrelay

import (
	"context"
	"time"
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"

	"github.com/chebyrash/promise"
)

const chainPollInterval = time.Minute * 10

type (
	Msg           *btcChainRelayMessage
	BtcChainRelay struct {
		p2p     *libp2p.P2PServer
		service libp2p.PubSubService[Msg]
		conf    common.IdentityConfig
		ctx     context.Context
	}

	btcChainRelayMessage struct{}
)

// Init implements aggregate.Plugin.
// Runs initialization in order of how they are passed in to `Aggregate`
func (b *BtcChainRelay) Init() error {
	return nil
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (b *BtcChainRelay) Start() *promise.Promise[any] {
	go func(b *BtcChainRelay) {
		ticker := time.NewTicker(chainPollInterval)
		defer ticker.Stop()

		select {

		case <-b.ctx.Done():
			return

		case <-ticker.C:
			b.poll()

		}
	}(b)

	return promise.New(func(resolve func(any), reject func(error)) {
		var err error

		b.service, err = libp2p.NewPubSubService(b.p2p, &p2pSpec{
			conf:   b.conf,
			oracle: b,
		})
		if err != nil {
			reject(err)
			return
		}

		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
// Runs cleanup once the `Aggregate` is finished
func (b *BtcChainRelay) Stop() error {
	if b.service == nil {
		return nil
	}
	return b.service.Close()
}

func New(
	p2pServer *libp2p.P2PServer,
	conf common.IdentityConfig,
) *BtcChainRelay {
	out := &BtcChainRelay{
		p2p:     p2pServer,
		service: nil,
		conf:    conf,
	}
	return out
}

func (b *BtcChainRelay) poll() {}
