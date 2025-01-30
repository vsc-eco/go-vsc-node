package data_availability

import (
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/announcements"
	libp2p "vsc-node/modules/p2p"

	"github.com/chebyrash/promise"
)

type DataAvailability struct {
	p2p     *libp2p.P2PServer
	service libp2p.PubSubService[p2pMessage]
	conf    announcements.AnnouncementsConfig
}

var _ a.Plugin = (*DataAvailability)(nil)

func New(p2p *libp2p.P2PServer, conf announcements.AnnouncementsConfig) *DataAvailability {
	return &DataAvailability{
		p2p:  p2p,
		conf: conf,
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
		<-d.service.Context().Done()
		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (d *DataAvailability) Stop() error {
	return d.stopP2P()
}
