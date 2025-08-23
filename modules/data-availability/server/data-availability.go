package data_availability_server

import (
	"vsc-node/lib/datalayer"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"
	start_status "vsc-node/modules/start-status"

	"github.com/chebyrash/promise"
)

type DataAvailability struct {
	p2p         *libp2p.P2PServer
	service     libp2p.PubSubService[p2pMessage]
	conf        common.IdentityConfig
	dl          *datalayer.DataLayer
	startStatus start_status.StartStatus
}

var _ a.Plugin = (*DataAvailability)(nil)

func New(p2p *libp2p.P2PServer, conf common.IdentityConfig, dl *datalayer.DataLayer) *DataAvailability {
	return &DataAvailability{
		p2p:         p2p,
		conf:        conf,
		dl:          dl,
		startStatus: start_status.New(),
	}
}

// Init implements aggregate.Plugin.
func (d *DataAvailability) Init() error {
	return nil
}

// Start implements aggregate.Plugin.
func (d *DataAvailability) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		err := d.startP2P()
		if err != nil {
			reject(err)
			return
		}
		// ctx, cancel := context.WithCancel(context.Background())
		// defer cancel()

		// fmt.Println("Metroid 2")
		// _, err = d.service.Started().Await(ctx)
		// fmt.Println("Metroid 3")
		// if err != nil {
		// 	d.startStatus.TriggerStartFailure(err)
		// 	reject(err)
		// 	return
		// }
		d.startStatus.TriggerStart()
		// fmt.Println("Metroid 4")
		// <-d.service.Context().Done()
		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (d *DataAvailability) Stop() error {
	return d.stopP2P()
}

func (d *DataAvailability) Started() *promise.Promise[any] {
	return d.startStatus.Started()
}
