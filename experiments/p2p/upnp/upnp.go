package upnp

import (
	"github.com/chebyrash/promise"
	"gitlab.com/NebulousLabs/go-upnp"

	"vsc-node/experiments/p2p/config"
	"vsc-node/modules/aggregate"
)

type UPnP struct {
	config *config.Config

	router   *upnp.IGD
	publicIP string
}

func New(config *config.Config) *UPnP {
	return &UPnP{config: config}
}

func (u *UPnP) PublicIpAddress() string {
	return u.publicIP
}

// Init implements aggregate.Plugin.
func (u *UPnP) Init() error {
	// connect to router
	d, err := upnp.Discover()
	if err != nil {
		return err
	}

	u.router = d

	// discover external IP
	ip, err := d.ExternalIP()
	if err != nil {
		return err
	}

	u.publicIP = ip

	// // record router's location
	// loc := d.Location()

	// // connect to router directly
	// d, err = upnp.Load(loc)
	// if err != nil {
	//     log.Fatal(err)
	// }

	return nil
}

// Start implements aggregate.Plugin.
func (u *UPnP) Start() *promise.Promise[any] {
	// forward a port
	return promise.New(func(resolve func(any), reject func(error)) {
		err := u.router.Forward(u.config.Port, "p2p service")
		if err != nil {
			reject(err)
		}
		resolve(nil)
	})
	// TODO change service name
}

// Stop implements aggregate.Plugin.
func (u *UPnP) Stop() error {
	// un-forward a port
	return u.router.Clear(u.config.Port)
}

var _ aggregate.Plugin = &UPnP{}
