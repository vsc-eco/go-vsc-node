package libp2p

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"
	"vsc-node/lib/utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/witnesses"
	start_status "vsc-node/modules/start-status"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
	libp2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/routing"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	multiaddr "github.com/multiformats/go-multiaddr"
	"github.com/robfig/cron/v3"

	// rpc "github.com/libp2p/go-libp2p-gorpc"
	// p "vsc-node/lib/pubsub"
	// "vsc-node/modules/aggregate"

	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
)

var MAINNET_BOOTSTRAP = []string{
	"/dnsaddr/api.vsc.eco/tcp/10720/p2p/12D3KooWFVVQ2xG6ohJ3tQrh3V6zZnRnKPVfQWVH5LjJakzBCs7E",
	"/ip4/149.56.25.168/tcp/10720/p2p/12D3KooWFVVQ2xG6ohJ3tQrh3V6zZnRnKPVfQWVH5LjJakzBCs7E",   // TODO this is api.vsc.eco, but DNS resolution doesn't work?
	"/ip4/173.211.12.65/tcp/10720/p2p/12D3KooWGpWrBc5pFx5GHWibczTPrazDCfk8GCETB5Ynb4Dq5L5V",   //@vaultec.vsc
	"/ip4/147.135.15.155/tcp/10720/p2p/12D3KooWCAE4XrkE4NJL3nqYkXXNhte94rdBDGGVQQJewrDXDVJZ",  // mengao
	"/ip4/188.40.125.182/tcp/10720/p2p/12D3KooWPzZ9RzsCP6BREUFbY1xyZiJ3PPoCW3DFGDhAwExiUazV",  //@spker
	"/ip4/37.27.190.82/tcp/10720/p2p/12D3KooWDEruPNvPqWc1DN7Fnj6euCZ5QN98YbK9v81wXA9aZqZr",    //@actifit.vsc
	"/ip4/51.75.151.131/tcp/10720/p2p/12D3KooWQnA1VE8H4QKAkeaEaXd7n1GpLxQXq1MoURn4Lnqhqe3X",   //@arcange.vsc
	"/ip4/185.130.45.196/tcp/10720/p2p/12D3KooWKUmixKkURVEktxNkn6Keb8LUPk4FdGXM6uAPGsiURw9z",  //@dalz.vsc
	"/ip4/5.78.133.3/tcp/10720/p2p/12D3KooWA8s64sSSbRLGGXspuU2hkfEkB7tKrcNDBWWjL6V8dC6o",      //@blocktrades.vsc
	"/ip4/185.130.45.154/tcp/10720/p2p/12D3KooWG4N5gCzbrX2k87baWtcYu2GgRxHvXJ1jmecx5cdxzohH",  //@v4vapp.vsc
	"/ip4/138.199.230.214/tcp/10720/p2p/12D3KooWHyT797hmbLjgbFjYFpbHrre1EPWzFo876FDhdcbPVG8p", //@tibfox.vsc
	// "/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	// "/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	// "/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	// "/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	// "/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",         // mars.i.ipfs.io
	// "/ip4/104.131.131.82/udp/4001/quic-v1/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ", // mars.i.ipfs.io
}

type P2PServer struct {
	witnessDb    WitnessGetter
	conf         common.IdentityConfig
	systemConfig common.SystemConfig

	host   host.Host
	dht    *kadDht.IpfsDHT
	pubsub *pubsub.PubSub
	cron   *cron.Cron

	startStatus start_status.StartStatus

	port int
}

var _ aggregate.Plugin = &P2PServer{}
var _ start_status.Starter = &P2PServer{}

type WitnessGetter interface {
	GetLastestWitnesses() ([]witnesses.Witness, error)
}

func New(witnessDb WitnessGetter, conf common.IdentityConfig, sconf common.SystemConfig, port ...int) *P2PServer {

	p := 10720
	if len(port) > 0 {
		p = port[0]
	}

	return &P2PServer{
		witnessDb:    witnessDb,
		conf:         conf,
		cron:         cron.New(),
		startStatus:  start_status.New(),
		port:         p,
		systemConfig: sconf,
	}
}

var topicNameFlag = "/vsc/mainnet/multicast"

// Finds VSC peers through DHT
func bootstrapVSCPeers(ctx context.Context, p2p *P2PServer) {
	h := p2p.host

	routingDiscovery := drouting.NewRoutingDiscovery(p2p.dht)
	dutil.Advertise(ctx, routingDiscovery, topicNameFlag)

	// Look for others who have announced and attempt to connect to them
	anyConnected := false
	for !anyConnected {

		// fmt.Println("Bootstraping peers via dht... PeerId: " + h.ID().String())
		peerChan, err := routingDiscovery.FindPeers(ctx, topicNameFlag)
		if err != nil {
			panic(err)
		}
		for peer := range peerChan {

			if peer.ID == h.ID() {
				continue // No self connection
			}
			p2p.host.Peerstore().AddAddrs(peer.ID, peer.Addrs, peerstore.ConnectedAddrTTL)
			err := h.Connect(ctx, peer)
			if err != nil {
				// fmt.Println("Failed connecting to ", peer.ID.String(), ", error:", err)
			} else {
				// fmt.Println("Connected to:", peer.ID.String())
				anyConnected = true
			}
		}
		time.Sleep(30 * time.Second)
	}
	fmt.Println("Bootstrap discovery complete")
}

// Started implements start_status.Starter.
func (p2pServer *P2PServer) Started() *promise.Promise[any] {
	return p2pServer.startStatus.Started()
}

// =================================
// ===== Plugin Implementation =====
// =================================

// Init implements aggregate.Plugin.
func (p2pServer *P2PServer) Init() error {
	//Future initialize using a configuration object with more detailed info
	key, err := p2pServer.conf.Libp2pPrivateKey()
	if err != nil {
		return err
	}

	ctx := context.Background()

	kadOptions := []kadDht.Option{
		kadDht.ProtocolPrefix("/vsc.network"),
	}

	if testing.Testing() {
		fmt.Println("In testing... running DHT in Server Mode")
		kadOptions = append(kadOptions, kadDht.Mode(kadDht.ModeServer))
	}

	var idht *dht.IpfsDHT
	options := []libp2p.Option{
		libp2p.ListenAddrStrings(fmt.Sprint("/ip4/0.0.0.0/tcp/", p2pServer.port)),
		libp2p.Identity(key),
		libp2p.EnableNATService(),
		libp2p.EnableRelayService(),
		libp2p.NATPortMap(),
		libp2p.EnableAutoNATv2(),
		libp2p.EnableHolePunching(),
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			idht, err = kadDht.New(context.Background(), h, kadOptions...)
			return idht, err
		}),
	}

	p2p, err := libp2p.New(options...)
	if err != nil {
		return err
	}

	//DHT wrapped host

	routedHost := rhost.Wrap(p2p, idht)
	p2pServer.host = routedHost
	p2pServer.dht = idht
	fmt.Println("peer ID:", p2pServer.PeerInfo().GetPeerId())

	go func() {
		cSub, _ := p2pServer.host.EventBus().Subscribe(new(event.EvtLocalReachabilityChanged))
		defer cSub.Close()

		select {
		case stat := <-cSub.Out():

			fmt.Println("NAT Status", stat)
		case <-time.After(30 * time.Second):

		}
	}()

	//Setup pubsub
	ps, err := pubsub.NewGossipSub(ctx, p2p, pubsub.WithDiscovery(drouting.NewRoutingDiscovery(p2pServer.dht)), pubsub.WithPeerExchange(true))
	if err != nil {
		return err
	}

	p2pServer.pubsub = ps

	return nil
}

// Start implements aggregate.Plugin.
func (p2ps *P2PServer) Start() *promise.Promise[any] {
	//What would we "start" for P2P?

	//Ask for P2P profiling from other nodes

	// send := make(chan HelloArgs)
	// reply := make(chan HelloReply)
	// ctx := context.Background()

	// err := p2ps.rpcClient.Stream(ctx, p2ps.host.ID(), "witness", "HelloWorld", send, reply)

	p2ps.cron.AddFunc("@every 5m", func() {
		p2ps.connectRegisteredPeers()
	})

	p2ps.cron.AddFunc("@every 5m", func() {
		p2ps.discoverPeers()
	})

	uniquePeers := make(map[string]struct{})

	if p2ps.systemConfig.Network == "mainnet" {
		for _, peerStr := range MAINNET_BOOTSTRAP {
			peerId, _ := peer.AddrInfoFromString(peerStr)
			uniquePeers[peerId.ID.String()] = struct{}{}
			p2ps.host.Peerstore().AddAddrs(peerId.ID, peerId.Addrs, peerstore.ConnectedAddrTTL)
			p2ps.host.Connect(context.Background(), *peerId)
		}
	}

	p2ps.dht.Bootstrap(context.Background())
	go bootstrapVSCPeers(context.Background(), p2ps)
	//First startup to try and get connected to the network
	go p2ps.connectRegisteredPeers()

	go func() {
		time.Sleep(50 * time.Millisecond)
		for {
			peerList := ""
			if len(p2ps.host.Network().Peers()) > 5 {
				for idx, peer := range p2ps.host.Network().Peers() {
					if idx >= 4 {
						break
					}
					if idx > 0 {
						peerList += " "
					}
					peerList += peer.String()
				}
				peerList += "..." + strconv.Itoa(len(p2ps.host.Network().Peers())-4) + " more"
			} else {
				for _, peer := range p2ps.host.Network().Peers() {
					peerList += peer.String() + ", "
				}
			}
			peerLen := len(p2ps.host.Network().Peers())
			// fmt.Println("peers", "["+peerList+"]", "peers.len()="+strconv.Itoa(peerLen))
			if peerLen >= len(uniquePeers)-1 {
				p2ps.startStatus.TriggerStart()
			}

			time.Sleep(5 * time.Second)
		}
	}()

	return utils.PromiseResolve[any](nil) //FIXME make this wait sub services to close
}

// Stop implements aggregate.Plugin.
func (p2p *P2PServer) Stop() error {
	//FIXME make this clean up sub services
	return nil
}

func (p2p *P2PServer) PeerInfo() common.PeerInfoGetter {
	return &peerGetter{
		server: p2p,
	}
}

func (p2p *P2PServer) BroadcastCidWithContext(ctx context.Context, cid cid.Cid) error {
	return p2p.dht.Provide(ctx, cid, true)
}

func (p2p *P2PServer) BroadcastCid(cid cid.Cid) error {
	return p2p.BroadcastCidWithContext(p2p.dht.Context(), cid)
}

// func (p2p *P2PServer) BroadcastCid(cid cid.Cid) error {
// 	return p2p.Dht.Provide(p2p.Dht.Context(), cid, true)
// }

// FindProvidersAsync implements routing.ContentDiscovery.
func (p2pServer *P2PServer) FindProvidersAsync(ctx context.Context, cid cid.Cid, count int) <-chan peer.AddrInfo {
	return p2pServer.dht.FindProvidersAsync(ctx, cid, count)
}

// Provide implements provider.Provide.
func (p2pServer *P2PServer) Provide(ctx context.Context, cid cid.Cid, broadcast bool) error {
	return p2pServer.dht.Provide(ctx, cid, broadcast)
}

// Addrs implements host.Host.
func (p2pServer *P2PServer) Addrs() []multiaddr.Multiaddr {
	return p2pServer.host.Addrs()
}

// Close implements host.Host.
func (p2pServer *P2PServer) Close() error {
	return p2pServer.host.Close()
}

// ConnManager implements host.Host.
func (p2pServer *P2PServer) ConnManager() connmgr.ConnManager {
	return p2pServer.host.ConnManager()
}

// Connect implements host.Host.
func (p2pServer *P2PServer) Connect(ctx context.Context, pi peer.AddrInfo) error {
	return p2pServer.host.Connect(ctx, pi)
}

// EventBus implements host.Host.
func (p2pServer *P2PServer) EventBus() event.Bus {
	return p2pServer.host.EventBus()
}

// ID implements host.Host.
func (p2pServer *P2PServer) ID() peer.ID {
	return p2pServer.host.ID()
}

// Mux implements host.Host.
func (p2pServer *P2PServer) Mux() protocol.Switch {
	return p2pServer.host.Mux()
}

// Network implements host.Host.
func (p2pServer *P2PServer) Network() network.Network {
	return p2pServer.host.Network()
}

// NewStream implements host.Host.
func (p2pServer *P2PServer) NewStream(ctx context.Context, p peer.ID, pids ...protocol.ID) (network.Stream, error) {
	return p2pServer.host.NewStream(ctx, p, pids...)
}

// Peerstore implements host.Host.
func (p2pServer *P2PServer) Peerstore() peerstore.Peerstore {
	return p2pServer.host.Peerstore()
}

// RemoveStreamHandler implements host.Host.
func (p2pServer *P2PServer) RemoveStreamHandler(pid protocol.ID) {
	p2pServer.host.RemoveStreamHandler(pid)
}

// SetStreamHandler implements host.Host.
func (p2pServer *P2PServer) SetStreamHandler(pid protocol.ID, handler network.StreamHandler) {
	p2pServer.host.SetStreamHandler(pid, handler)
}

// SetStreamHandlerMatch implements host.Host.
func (p2pServer *P2PServer) SetStreamHandlerMatch(pid protocol.ID, matcher func(protocol.ID) bool, handler network.StreamHandler) {
	p2pServer.host.SetStreamHandlerMatch(pid, matcher, handler)
}

func (p2p *P2PServer) connectRegisteredPeers() {
	witnesses, _ := p2p.witnessDb.GetLastestWitnesses()

	for _, witness := range witnesses {
		if witness.PeerId == "" {
			continue
		}
		peerId, _ := peer.AddrInfoFromString("/p2p/" + witness.PeerId)

		for _, peer := range p2p.host.Network().Peers() {
			if peer.String() == peerId.ID.String() {
				p2p.host.Peerstore().AddAddrs(peerId.ID, peerId.Addrs, peerstore.ConnectedAddrTTL)
				p2p.host.Connect(context.Background(), *peerId)
			}
		}
		p2p.host.Connect(context.Background(), *peerId)
	}
}

func (p2p *P2PServer) discoverPeers() {

	if len(p2p.host.Network().Peers()) < 2 {
		if p2p.systemConfig.Network == "mainnet" {
			for _, peerStr := range MAINNET_BOOTSTRAP {
				peerId, _ := peer.AddrInfoFromString(peerStr)

				p2p.host.Connect(context.Background(), *peerId)
			}
		}
	}

	ctx := context.Background()

	h := p2p.host

	routingDiscovery := drouting.NewRoutingDiscovery(p2p.dht)
	dutil.Advertise(ctx, routingDiscovery, topicNameFlag)

	// Look for others who have announced and attempt to connect to them
	// fmt.Println("Searching for peers via dht...")

	peerChan, err := routingDiscovery.FindPeers(ctx, topicNameFlag)
	if err != nil {
		panic(err)
	}
	for peer := range peerChan {
		if peer.ID == h.ID() {
			continue // No self connection
		}
		p2p.host.Peerstore().AddAddrs(peer.ID, peer.Addrs, peerstore.ConnectedAddrTTL)
		h.Connect(ctx, peer)
	}
}

type RPCService struct {
	p2pService *P2PServer
}

type HelloArgs struct {
	Msg string
}

type HelloReply struct {
	Msg string
}

func (svc *RPCService) HelloWorld(ctx context.Context, argType <-chan HelloArgs, HelloArgs chan<- HelloReply) error {

	fmt.Println("Being called Hello World")

	for {
		m, more := <-argType
		if more {
			fmt.Println(m, more)
		} else {
			break
		}
		// var message *HelloArgs
		// message <- argType
		// fmt.Println("Through Stream", message)
		// replyType <- &HelloReply{
		// 	Msg: message.Msg,
		// }
	}

	return errors.New("uh oh")
}

type SignBlockAsk struct {
	SlotHeight int64
}

type SignBlockResponse struct {
	Hash      []byte
	Signature string
}

func (svc *RPCService) SignBlock(ctx context.Context, signAsk SignBlockAsk, signResponse *SignBlockResponse) error {
	return nil
}

type GetBlockSigsAsk struct {
	SlotHeight int64
}

type Signature struct {
	Username string
	Sig      string
}

type GetBlockSigsResponse struct {
	Signatures []Signature
	BitVector  []byte
}

func (svc *RPCService) GetBlockSignatures(ctx context.Context, ask GetBlockSigsAsk, res *GetBlockSigsResponse) error {

	return nil
}

type PushBlockSignatureAsk struct {
	SlotHeight int64
	Signatures []Signature
}

type PushBlockSignatureResponse struct {
	ok bool
}

/**
* Push block signatures to node.
* If not asking for signatures of a specific slot height, then apushes will be rejected
*
 */
func (svc *RPCService) PushBlockSignature(ctx context.Context, ask PushBlockSignatureAsk, res *PushBlockSignatureResponse) error {

	return nil
}

// =================================
// ===== PubSub Implementation =====
// =================================

// Peers implements pubsub.PubSub.
func (l *P2PServer) Peers() []peer.ID {
	panic("Unimplemented")
}

// SendTo implements pubsub.PubSub.
func (l *P2PServer) SendTo(topic string, message []byte, recipients []peer.ID) {
	panic("unimplemented")
}

// SendToAll implements pubsub.PubSub.
func (l *P2PServer) SendToAll(topic string, message []byte) {
	panic("unimplemented")
}
