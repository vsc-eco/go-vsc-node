package libp2p

import (
	"net"
	"testing"

	"github.com/multiformats/go-multiaddr"
)

type peerGetter struct {
	server *P2PServer
}

func (pg *peerGetter) GetPeerId() string {
	return pg.server.host.ID().String()
}

func (pg *peerGetter) GetPeerAddr() multiaddr.Multiaddr {
	addrs := pg.server.host.Addrs()
	return addrs[0]
}
func (pg *peerGetter) GetPeerAddrs() []multiaddr.Multiaddr {
	addrs := pg.server.host.Addrs()
	return addrs
}

func (pg *peerGetter) GetStatus() {

}

// var _ common_types.PeerInfoGetter = &peerGetter{}

func isPublicAddr(addr multiaddr.Multiaddr) bool {
	if testing.Testing() {
		// assume nodes in e2e tests are reachable regardless
		return true
	}
	ipv4Address, err := addr.ValueForProtocol(multiaddr.P_IP4)

	ip := net.ParseIP(ipv4Address)
	return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalMulticast() && !ip.IsLinkLocalUnicast() && err == nil
}

func isCircuitAddr(addr multiaddr.Multiaddr) bool {
	_, err := addr.ValueForProtocol(multiaddr.P_CIRCUIT)

	return err == nil
}
