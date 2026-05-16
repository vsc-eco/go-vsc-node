package libp2p

import (
	"testing"

	libp2pNet "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// Pentest finding N-L6 — pin the gater's deny semantics so a
// future regression to "always allow" trips the test.

type fakeConnAddrs struct {
	local, remote ma.Multiaddr
}

func (f fakeConnAddrs) LocalMultiaddr() ma.Multiaddr  { return f.local }
func (f fakeConnAddrs) RemoteMultiaddr() ma.Multiaddr { return f.remote }

var _ libp2pNet.ConnMultiaddrs = fakeConnAddrs{}

func TestNL6_ConnectionGaterBlockPeer(t *testing.T) {
	g := newConnectionGater()
	pid := peer.ID("12D3KooWBlocked")

	if !g.InterceptPeerDial(pid) {
		t.Fatal("default state: dial to unblocked peer must be allowed")
	}
	g.BlockPeer(pid)
	if g.InterceptPeerDial(pid) {
		t.Fatal("N-L6 leak: blocked peer was still allowed to dial")
	}

	g.UnblockPeer(pid)
	if !g.InterceptPeerDial(pid) {
		t.Fatal("UnblockPeer should restore allow-state")
	}
}

func TestNL6_ConnectionGaterBlockSubnet(t *testing.T) {
	g := newConnectionGater()
	if err := g.BlockSubnet("10.0.0.0/24"); err != nil {
		t.Fatalf("BlockSubnet: %v", err)
	}

	inSubnet, _ := ma.NewMultiaddr("/ip4/10.0.0.42/tcp/4002")
	outSubnet, _ := ma.NewMultiaddr("/ip4/10.0.1.42/tcp/4002")

	if g.InterceptAccept(fakeConnAddrs{remote: inSubnet}) {
		t.Fatal("N-L6 leak: subnet-blocked peer was accepted")
	}
	if !g.InterceptAccept(fakeConnAddrs{remote: outSubnet}) {
		t.Fatal("subnet-allowed peer was wrongly rejected")
	}
}

func TestNL6_ConnectionGaterIPv6Subnet(t *testing.T) {
	g := newConnectionGater()
	if err := g.BlockSubnet("2001:db8::/32"); err != nil {
		t.Fatalf("IPv6 BlockSubnet: %v", err)
	}
	in, _ := ma.NewMultiaddr("/ip6/2001:db8:1234::1/tcp/4002")
	if g.InterceptAccept(fakeConnAddrs{remote: in}) {
		t.Fatal("IPv6 subnet-blocked peer was accepted")
	}
}

func TestNL6_ConnectionGaterMalformedCIDRRejected(t *testing.T) {
	g := newConnectionGater()
	if err := g.BlockSubnet("not-a-cidr"); err == nil {
		t.Fatal("BlockSubnet should reject malformed CIDR")
	}
}
