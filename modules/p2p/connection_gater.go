package libp2p

import (
	"net"
	"strings"
	"sync"

	"github.com/libp2p/go-libp2p/core/control"
	libp2pNet "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// Pentest finding N-L6: the libp2p host had no ConnectionGater,
// so every inbound dial / accept was unconditional. A
// ConnectionGater gives operators a single hook to refuse
// connections from specific peer IDs or address ranges (e.g.
// after observing abuse, before a take-down) without restarting
// the node.
//
// The gater here is a thin opt-in: by default it accepts
// everything, matching the prior behaviour. Operators populate
// the deny lists at runtime via BlockPeer / BlockSubnet to ban
// without a node restart.

type p2pConnectionGater struct {
	mu             sync.RWMutex
	blockedPeers   map[peer.ID]struct{}
	blockedSubnets []*net.IPNet
}

func newConnectionGater() *p2pConnectionGater {
	return &p2pConnectionGater{
		blockedPeers: map[peer.ID]struct{}{},
	}
}

// connectionGater is the lazy accessor used by the libp2p option
// builder. Returns the same instance on subsequent calls so
// runtime-applied BlockPeer/BlockSubnet calls take effect.
func (p *P2PServer) connectionGater() *p2pConnectionGater {
	if p.gater == nil {
		p.gater = newConnectionGater()
	}
	return p.gater
}

// BlockPeer adds a peer ID to the deny list. Future inbound and
// outbound connections to/from this peer are refused.
func (g *p2pConnectionGater) BlockPeer(p peer.ID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blockedPeers[p] = struct{}{}
}

// UnblockPeer removes a peer ID from the deny list.
func (g *p2pConnectionGater) UnblockPeer(p peer.ID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.blockedPeers, p)
}

// BlockSubnet adds a CIDR range to the deny list. Returns the
// parse error if cidr is malformed.
func (g *p2pConnectionGater) BlockSubnet(cidr string) error {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blockedSubnets = append(g.blockedSubnets, n)
	return nil
}

func (g *p2pConnectionGater) isPeerBlocked(p peer.ID) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.blockedPeers[p]
	return ok
}

func (g *p2pConnectionGater) isAddrBlocked(addr ma.Multiaddr) bool {
	if addr == nil {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.blockedSubnets) == 0 {
		return false
	}
	ip := extractIP(addr)
	if ip == nil {
		return false
	}
	for _, n := range g.blockedSubnets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// connmgr.ConnectionGater interface implementation. All five
// methods consult the same allow/deny lists.

func (g *p2pConnectionGater) InterceptPeerDial(p peer.ID) bool {
	return !g.isPeerBlocked(p)
}

func (g *p2pConnectionGater) InterceptAddrDial(p peer.ID, addr ma.Multiaddr) bool {
	return !g.isPeerBlocked(p) && !g.isAddrBlocked(addr)
}

func (g *p2pConnectionGater) InterceptAccept(cma libp2pNet.ConnMultiaddrs) bool {
	return !g.isAddrBlocked(cma.RemoteMultiaddr())
}

func (g *p2pConnectionGater) InterceptSecured(_ libp2pNet.Direction, p peer.ID, cma libp2pNet.ConnMultiaddrs) bool {
	return !g.isPeerBlocked(p) && !g.isAddrBlocked(cma.RemoteMultiaddr())
}

func (g *p2pConnectionGater) InterceptUpgraded(_ libp2pNet.Conn) (bool, control.DisconnectReason) {
	return true, 0
}

// extractIP pulls an IPv4/IPv6 component out of a multiaddr. Returns
// nil for non-IP transports (relay circuits, unix sockets, etc.).
func extractIP(addr ma.Multiaddr) net.IP {
	for _, p := range addr.Protocols() {
		switch p.Code {
		case ma.P_IP4, ma.P_IP6:
			s, err := addr.ValueForProtocol(p.Code)
			if err != nil {
				continue
			}
			if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil {
				return ip
			}
		}
	}
	return nil
}
