package datalayer_test

import (
	"context"
	"testing"
	"time"

	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"

	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

type startupNode struct {
	p2p *p2pInterface.P2PServer
	da  *DataLayer.DataLayer
}

// newStartupNode builds a mocknet node (identity, p2p, datalayer) on a fixed local
// port (the advertised addresses carry the configured port, so 0 is not dialable)
// without running any plugin lifecycle step; the test drives Init and Start itself.
func newStartupNode(t *testing.T, port int) startupNode {
	t.Helper()
	dir := t.TempDir()
	identityConfig := common.NewIdentityConfig(dir)
	require.NoError(t, identityConfig.Init())
	p2pConfig := p2pInterface.NewConfig(dir)
	require.NoError(t, p2pConfig.Init())
	require.NoError(t, p2pConfig.SetOptions(p2pInterface.P2POpts{
		Port:         port,
		ServerMode:   true,
		AllowPrivate: true,
		Bootnodes:    []string{},
	}))
	p2p := p2pInterface.New(witnesses.NewEmptyWitnesses(), p2pConfig, identityConfig, systemconfig.MocknetConfig(), nil)
	return startupNode{p2p: p2p, da: DataLayer.New(p2p, dir)}
}

// P2P-START-1: a peer that connects while a node is still starting must not leave
// that node unable to fetch from it. The node's p2p host exists after p2p Init, but
// bitswap only registers in the datalayer's Init, and boxo's bitswap never learns of a
// connection made before it started. The connection survives (a later Connect is a
// no-op), so without the startup hold the node can never fetch from that peer.
func TestP2PStartupWindow_EarlyPeerCanStillServeBlocks(t *testing.T) {
	ctx := context.Background()

	// B: a peer that is fully up and holds a block.
	b := newStartupNode(t, 29731)
	require.NoError(t, b.p2p.Init())
	require.NoError(t, b.da.Init())
	_, err := b.p2p.Start().Await(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { b.p2p.Stop(); b.da.Stop(); b.p2p.Close() })
	node := merkledag.NodeWithData([]byte("p2p-start-1 block"))
	require.NoError(t, b.da.DagServ.Add(ctx, node))

	// A: restarting. Its host is up (p2p Init) but its datalayer is not.
	a := newStartupNode(t, 29732)
	require.NoError(t, a.p2p.Init())
	t.Cleanup(func() { a.p2p.Stop(); a.da.Stop(); a.p2p.Close() })

	// B reaches A inside the window, as a peer that redials a restarted node does.
	aInfo := peer.AddrInfo{ID: a.p2p.ID(), Addrs: a.p2p.Addrs()}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	earlyErr := b.p2p.Connect(dialCtx, aInfo)
	cancel()
	t.Logf("B -> A during A's startup: err=%v", earlyErr)
	time.Sleep(500 * time.Millisecond)
	// A QUIC dial can report success before A's gater closes it; what matters is that
	// A keeps no connection from inside the window.
	require.Equal(t, network.NotConnected, a.p2p.Network().Connectedness(b.p2p.ID()),
		"A kept a connection made before its datalayer was up")

	// A finishes starting the way a node does: the remaining Inits, then Start, then
	// it dials its peers.
	require.NoError(t, a.da.Init())
	_, err = a.p2p.Start().Await(ctx)
	require.NoError(t, err)
	bInfo := peer.AddrInfo{ID: b.p2p.ID(), Addrs: b.p2p.Addrs()}
	dialCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	require.NoError(t, a.p2p.Connect(dialCtx, bInfo))
	cancel()

	getCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	got, err := a.da.DagServ.Get(getCtx, node.Cid())
	require.NoError(t, err, "A could not fetch B's block after %s: bitswap never saw the connection B made during A's startup", time.Since(start))
	require.Equal(t, node.RawData(), got.RawData())
	t.Logf("A fetched B's block in %s", time.Since(start))
}
