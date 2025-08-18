package oracle

import (
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"

	"github.com/chebyrash/promise"
)

const topic = "/vsc/mainet/oracle/v1"

type (
	Msg    *oracleMessage
	Oracle struct {
		p2p     *libp2p.P2PServer
		service libp2p.PubSubService[Msg]
		conf    common.IdentityConfig
	}

	oracleMessage struct{}
)

// Init implements aggregate.Plugin.
// Runs initialization in order of how they are passed in to `Aggregate`
func (o *Oracle) Init() error {
	return nil
}

// Start implements aggregate.Plugin.
// Runs startup and should be non blocking
func (o *Oracle) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		var err error

		o.service, err = libp2p.NewPubSubService(o.p2p, &p2pSpec{
			conf:   o.conf,
			oracle: o,
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
func (o *Oracle) Stop() error {
	if o.service == nil {
		return nil
	}
	return o.service.Close()
}

func New(p2pServer *libp2p.P2PServer, conf common.IdentityConfig) *Oracle {
	out := &Oracle{
		p2p:     p2pServer,
		service: nil,
		conf:    conf,
	}
	return out
}
