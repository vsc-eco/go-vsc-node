package libp2p

import (
	"net"

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
	// Check IPv4 address
	ipv4Address, err := addr.ValueForProtocol(multiaddr.P_IP4)
	if err == nil {
		ip := net.ParseIP(ipv4Address)
		return ip != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalMulticast() && !ip.IsLinkLocalUnicast()
	}

	// Check IPv6 address
	ipv6Address, err := addr.ValueForProtocol(multiaddr.P_IP6)
	if err == nil {
		ip := net.ParseIP(ipv6Address)
		return ip != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalMulticast() && !ip.IsLinkLocalUnicast()
	}

	return false
}

func isCircuitAddr(addr multiaddr.Multiaddr) bool {
	_, err := addr.ValueForProtocol(multiaddr.P_CIRCUIT)

	return err == nil
}
