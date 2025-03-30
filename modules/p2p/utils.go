package libp2p

import (
	"vsc-node/modules/common"

	"github.com/multiformats/go-multiaddr"
)

type peerGetter struct {
	server *P2PServer
}

func (pg *peerGetter) GetPeerId() string {
	return pg.server.Host.ID().String()
}

func (pg *peerGetter) GetPeerAddr() multiaddr.Multiaddr {
	addrs := pg.server.Host.Addrs()
	return addrs[0]
}
func (pg *peerGetter) GetPeerAddrs() []multiaddr.Multiaddr {
	addrs := pg.server.Host.Addrs()
	return addrs
}

var _ common.PeerInfoGetter = &peerGetter{}
