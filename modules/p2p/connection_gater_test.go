package libp2p

import (
	"crypto/rand"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
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

func TestNL6_ApplyConfigSeedsDenyLists(t *testing.T) {
	priv, pub, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	_ = priv
	pid, err := peer.IDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("peer id: %v", err)
	}

	g := newConnectionGater()
	nPeers, nSubnets, errs := g.applyConfig(
		[]string{pid.String(), "  ", "not-a-peer-id"},
		[]string{"10.0.0.0/24", "", "not-a-cidr"},
	)

	if nPeers != 1 {
		t.Fatalf("expected 1 peer applied, got %d", nPeers)
	}
	if nSubnets != 1 {
		t.Fatalf("expected 1 subnet applied, got %d", nSubnets)
	}
	if len(errs) != 2 {
		t.Fatalf("expected 2 parse errors (bad peer + bad cidr), got %d: %v", len(errs), errs)
	}
	if g.InterceptPeerDial(pid) {
		t.Fatal("N-L6 leak: config-blocked peer was still allowed to dial")
	}
	inSubnet, _ := ma.NewMultiaddr("/ip4/10.0.0.7/tcp/4002")
	if g.InterceptAccept(fakeConnAddrs{remote: inSubnet}) {
		t.Fatal("N-L6 leak: config-blocked subnet was accepted")
	}
}

func TestNL6_ConnectionGaterMalformedCIDRRejected(t *testing.T) {
	g := newConnectionGater()
	if err := g.BlockSubnet("not-a-cidr"); err == nil {
		t.Fatal("BlockSubnet should reject malformed CIDR")
	}
}

// P2P-START-1: the startup hold refuses every connection, in and out, until it is
// released, and the deny lists keep working once it is.
func TestP2PStart1_GaterHoldsEveryConnectionUntilRelease(t *testing.T) {
	g := newConnectionGater()
	pid := peer.ID("12D3KooWStarting")
	other := peer.ID("12D3KooWBlockedAfter")
	addr := ma.StringCast("/ip4/192.0.2.7/tcp/10720")
	cma := fakeConnAddrs{local: ma.StringCast("/ip4/0.0.0.0/tcp/10720"), remote: addr}

	allowed := func() []bool {
		return []bool{
			g.InterceptPeerDial(pid),
			g.InterceptAddrDial(pid, addr),
			g.InterceptAccept(cma),
			g.InterceptSecured(libp2pNet.DirInbound, pid, cma),
			g.InterceptSecured(libp2pNet.DirOutbound, pid, cma),
		}
	}

	g.holdUntilStart()
	for i, ok := range allowed() {
		if ok {
			t.Fatalf("hold: intercept %d let a connection through before Start", i)
		}
	}

	if !g.releaseHold() {
		t.Fatal("releaseHold must report the hold it lifted")
	}
	if g.releaseHold() {
		t.Fatal("a second releaseHold has no hold to lift")
	}
	for i, ok := range allowed() {
		if !ok {
			t.Fatalf("released: intercept %d still refuses", i)
		}
	}

	g.BlockPeer(other)
	if g.InterceptPeerDial(other) {
		t.Fatal("deny list must still apply after the hold is released")
	}
}
