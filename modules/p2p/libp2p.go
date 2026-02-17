package libp2p

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"vsc-node/lib/utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	systemconfig "vsc-node/modules/common/system-config"
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
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	multiaddr "github.com/multiformats/go-multiaddr"
	"github.com/robfig/cron/v3"

	// rpc "github.com/libp2p/go-libp2p-gorpc"
	// p "vsc-node/lib/pubsub"
	// "vsc-node/modules/aggregate"

	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
)

type P2PServer struct {
	witnessDb    WitnessGetter
	conf         common.IdentityConfig
	systemConfig systemconfig.SystemConfig

	host   host.Host
	dht    *kadDht.IpfsDHT
	pubsub *pubsub.PubSub
	cron   *cron.Cron

	startStatus start_status.StartStatus
	blockStatus common_types.BlockStatusGetter

	port int

	reachabilityStatus network.Reachability
}

var _ aggregate.Plugin = &P2PServer{}
var _ start_status.Starter = &P2PServer{}

type WitnessGetter interface {
	GetLastestWitnesses(...witnesses.SearchOption) ([]witnesses.Witness, error)
}

func New(witnessDb WitnessGetter, conf common.IdentityConfig, sconf systemconfig.SystemConfig, blockStatus common_types.BlockStatusGetter, port ...int) *P2PServer {

	p := 10720
	if len(port) > 0 {
		p = port[0]
	}

	return &P2PServer{
		witnessDb:    witnessDb,
		conf:         conf,
		cron:         cron.New(),
		startStatus:  start_status.New(),
		blockStatus:  blockStatus,
		port:         p,
		systemConfig: sconf,
	}
}

// topicName returns the pubsub topic for this network (e.g. /vsc/mainnet/multicast or /vsc/testnet/multicast)
func (p2p *P2PServer) topicName() string {
	net := strings.TrimPrefix(p2p.systemConfig.NetId(), "vsc-")
	return "/vsc/" + net + "/multicast"
}

// Finds VSC peers through DHT
func bootstrapVSCPeers(ctx context.Context, p2p *P2PServer) {
	h := p2p.host
	topic := p2p.topicName()

	routingDiscovery := drouting.NewRoutingDiscovery(p2p.dht)
	dutil.Advertise(ctx, routingDiscovery, topic)

	// Look for others who have announced and attempt to connect to them
	anyConnected := false
	for !anyConnected {

		// fmt.Println("Bootstraping peers via dht... PeerId: " + h.ID().String())
		peerChan, err := routingDiscovery.FindPeers(ctx, topic)
		if err != nil {
			panic(err)
		}
		for peer := range peerChan {

			if peer.ID == h.ID() {
				continue // No self connection
			}
			// p2p.host.Peerstore().AddAddrs(peer.ID, peer.Addrs, peerstore.ConnectedAddrTTL)
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

	bootstrapPeers := []peer.AddrInfo{}
	//fill it up from system config
	for _, peerStr := range p2pServer.systemConfig.BootstrapPeers() {
		peerId, err := peer.AddrInfoFromString(peerStr)
		if err != nil {
			fmt.Println("Error parsing bootstrap peer:", peerStr, err)
			continue
		}
		bootstrapPeers = append(bootstrapPeers, *peerId)
	}
	kadOptions := []kadDht.Option{
		kadDht.ProtocolPrefix("/vsc.network"),
		kadDht.BootstrapPeers(bootstrapPeers...),
	}

	if testing.Testing() {
		fmt.Println("In testing... running DHT in Server Mode")
		kadOptions = append(kadOptions, kadDht.Mode(kadDht.ModeServer))
	}

	relayResources := relay.DefaultResources()
	relayResources.MaxCircuits = 128
	relayResources.ReservationTTL = 8 * time.Hour

	var idht *dht.IpfsDHT
	options := []libp2p.Option{
		libp2p.ListenAddrStrings(
			fmt.Sprint("/ip4/0.0.0.0/udp/", p2pServer.port, "/quic-v1"),
			fmt.Sprint("/ip4/0.0.0.0/tcp/", p2pServer.port),
		),
		libp2p.Identity(key),
		libp2p.EnableNATService(),
		libp2p.EnableRelayService(relay.WithInfiniteLimits(), relay.WithResources(relayResources)),
		libp2p.NATPortMap(),
		libp2p.EnableAutoNATv2(),
		libp2p.EnableHolePunching(),
		libp2p.EnableAutoRelayWithPeerSource(func(ctx context.Context, numPeers int) <-chan peer.AddrInfo {
			c := make(chan peer.AddrInfo, numPeers)

			if p2pServer.host != nil {
				for _, peer := range p2pServer.host.Network().Peers() {
					addrInfo := p2pServer.host.Peerstore().PeerInfo(peer)
					var goodPeer bool
					for _, a := range addrInfo.Addrs {
						if isPublicAddr(a) {
							goodPeer = true
						}
					}

					if goodPeer {
						c <- addrInfo
					}
				}
			}

			return c
		}, autorelay.WithMaxCandidateAge(5*time.Minute)),
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			idht, err = kadDht.New(context.Background(), h, kadOptions...)
			return idht, err
		}),
		libp2p.AddrsFactory(p2pServer.addrFactory),
	}

	p2p, err := libp2p.New(options...)
	if err != nil {
		return err
	}

	routedHost := rhost.Wrap(p2p, idht)
	p2pServer.host = routedHost
	p2pServer.dht = idht
	fmt.Println("peer ID:", p2pServer.GetPeerId())

	go func() {
		cSub, _ := p2pServer.host.EventBus().Subscribe(new(event.EvtLocalReachabilityChanged))
		defer cSub.Close()

		select {
		case stat := <-cSub.Out():

			fmt.Println("NAT Status", stat)
			p2pServer.reachabilityStatus = stat.(event.EvtLocalReachabilityChanged).Reachability
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

	// go func() {
	// 	for {
	// 		time.Sleep(10 * time.Second)
	// 		p2ps.host.ID()

	// 		pps := p2ps.host.Mux().Protocols()

	// 		fmt.Println("pps, err", pps, p2ps.host.Peerstore().PeerInfo(p2ps.host.ID()))
	// 		peers := p2ps.host.Network().Peers()
	// 		fmt.Println("Connected Peers:", peers)
	// 		fmt.Println("IGV", len(peers))

	// 		peerID, _ := peer.Decode("12D3KooWRnteMca3kEjGeSB7hDQNhD4RaVK8Ls5cJkzimmpo8VK6")
	// 		pps2, err := p2ps.dht.FindPeer(context.Background(), peerID)

	// 		fmt.Println("FindPeer", pps2, err)
	// 		peerInfo := p2ps.host.Peerstore().PeerInfo(peerID)
	// 		fmt.Println("PeerInfo", peerInfo)
	// 		err = p2ps.host.Connect(context.Background(), peerInfo)

	// 		fmt.Println("Connect", err)
	// 		protocols, _ := p2ps.host.Peerstore().GetProtocols(peerID)
	// 		fmt.Println("Protocols", protocols)
	// 		ctx, cancel := context.WithCancel(context.Background())
	// 		res := ping.NewPingService(p2ps.host).Ping(ctx, peerID)

	// 		go func() {
	// 			for {
	// 				pong, ok := <-res
	// 				if !ok {
	// 					return
	// 				}
	// 				if pong.Error == nil {
	// 					fmt.Println("Pinged", peerID.String(), "in", pong.RTT.Nanoseconds(), ok, pong.Error)
	// 					cancel()
	// 					break
	// 				} else {
	// 					fmt.Println("Ping error", pong.Error)
	// 				}
	// 			}
	// 		}()

	// 		fmt.Println("My addresses", p2ps.host.Addrs())
	// 		fmt.Println("Registered addresses", p2ps.host.Peerstore().Addrs(p2ps.host.ID()))
	//
	// 	}
	// }()

	// p2ps.cron.AddFunc("@every 5m", func() {
	// 	p2ps.connectRegisteredPeers()
	// })

	p2ps.cron.AddFunc("@every 30m", func() {
		p2ps.discoverPeers()
	})

	p2ps.cron.AddFunc("@every 15m", func() {
		p2ps.advertiseSelf()

	})

	go func() {
		time.Sleep(1 * time.Minute)
		p2ps.advertiseSelf()
	}()

	uniquePeers := make(map[string]struct{})

	if p2ps.systemConfig.OnMainnet() {
		for _, peerStr := range p2ps.systemConfig.BootstrapPeers() {
			peerId, _ := peer.AddrInfoFromString(peerStr)
			err := p2ps.host.Connect(context.Background(), *peerId)
			if err == nil {
				uniquePeers[peerId.ID.String()] = struct{}{}
				p2ps.host.Peerstore().AddAddrs(peerId.ID, peerId.Addrs, peerstore.ConnectedAddrTTL)

			}
		}
	}

	err := p2ps.dht.Bootstrap(context.Background())
	fmt.Println("DHT Bootstrap error:", err)

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

	p2ps.cron.Start()

	return utils.PromiseResolve[any](nil) //FIXME make this wait sub services to close
}

// Stop implements aggregate.Plugin.
func (p2p *P2PServer) Stop() error {
	//FIXME make this clean up sub services
	return nil
}

func (pg *P2PServer) GetPeerId() string {
	return pg.host.ID().String()
}

func (pg *P2PServer) GetPeerAddr() multiaddr.Multiaddr {
	addrs := pg.host.Addrs()
	return addrs[0]
}
func (pg *P2PServer) GetPeerAddrs() []multiaddr.Multiaddr {
	addrs := pg.host.Addrs()
	return addrs
}

func (p2p *P2PServer) GetStatus() network.Reachability {
	return p2p.reachabilityStatus
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

func (p2p *P2PServer) addrFactory(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
	filteredAddrs := make([]multiaddr.Multiaddr, 0)
	var publicAddr multiaddr.Multiaddr
	for _, addr := range addrs {
		if isCircuitAddr(addr) {
			filteredAddrs = append(filteredAddrs, addr)
		}
		if isPublicAddr(addr) {
			publicAddr = addr
		}
	}

	if publicAddr != nil {
		ipAddr, err := publicAddr.ValueForProtocol(multiaddr.P_IP4)
		if err == nil {
			ma1, _ := multiaddr.NewMultiaddr("/ip4/" + ipAddr + "/tcp/" + strconv.Itoa(p2p.port))
			filteredAddrs = append(filteredAddrs, ma1)
			ma2, _ := multiaddr.NewMultiaddr("/ip4/" + ipAddr + "/udp/" + strconv.Itoa(p2p.port) + "/quic-v1")
			filteredAddrs = append(filteredAddrs, ma2)
		}
	}

	return filteredAddrs
}

func (p2p *P2PServer) connectRegisteredPeers() {
	opts := []witnesses.SearchOption{}

	// if p2p.blockStatus != nil {
	// 	currentBlockHeight := p2p.blockStatus.BlockHeight()
	// 	opts = append(opts, witnesses.SearchHeight(currentBlockHeight))
	// 	opts = append(opts, witnesses.SearchExpiration(witnesses.WITNESS_EXPIRE_BLOCKS))
	// }

	fmt.Println("connectRegisteredPeers opts", opts)

	witnesses, _ := p2p.witnessDb.GetLastestWitnesses(opts...)

	for _, witness := range witnesses {
		if witness.PeerId == "" {
			continue
		}
		witnessTime, _ := time.Parse(time.RFC3339, witness.Ts)
		//Check if witnessTime is 4 days old or longer
		if time.Since(witnessTime) > time.Hour*24*4 {
			continue
		}
		mp, err := multiaddr.NewMultiaddr("/p2p/" + witness.PeerId)

		if err != nil {
			continue
		}

		var selectedAddr []multiaddr.Multiaddr
		//Select
		for _, peer := range witness.PeerAddrs {
			m, _ := multiaddr.NewMultiaddr(peer)

			// fmt.Println("circuitAddress", circuitAddress, err)
			if isCircuitAddr(m) {
				selectedAddr = append(selectedAddr, m.Encapsulate(mp))
				continue
			}

			if isPublicAddr(m) {
				selectedAddr = append(selectedAddr, m.Encapsulate(mp))
				continue
			}
		}

		peerId, _ := peer.AddrInfoFromString("/p2p/" + witness.PeerId)

		for _, peer := range p2p.host.Network().Peers() {
			if peer.String() == peerId.ID.String() {
				p2p.host.Peerstore().AddAddrs(peerId.ID, peerId.Addrs, peerstore.ConnectedAddrTTL)
				p2p.host.Connect(context.Background(), *peerId)
			}
		}
		addrInfo, err := peer.AddrInfosFromP2pAddrs(selectedAddr...)
		if err != nil && len(addrInfo) > 0 {
			p2p.host.Connect(context.Background(), addrInfo[0])
		}
	}
}

func (p2p *P2PServer) discoverPeers() {
	fmt.Println("Discovering peers...")
	if len(p2p.host.Network().Peers()) < 2 {
		if p2p.systemConfig.OnMainnet() {
			for _, peerStr := range p2p.systemConfig.BootstrapPeers() {
				peerId, _ := peer.AddrInfoFromString(peerStr)

				p2p.host.Connect(context.Background(), *peerId)
			}
		}
	}

	ctx := context.Background()

	h := p2p.host

	routingDiscovery := drouting.NewRoutingDiscovery(p2p.dht)

	// Look for others who have announced and attempt to connect to them
	// fmt.Println("Searching for peers via dht...")

	peerChan, err := routingDiscovery.FindPeers(ctx, p2p.topicName())
	if err != nil {
		panic(err)
	}
	for {
		peer := <-peerChan
		if peer.ID == h.ID() {
			continue // No self connection
		}
		if peer.ID == "" {
			break
		}

		err := h.Connect(ctx, peer)
		if err == nil {
			p2p.host.Peerstore().AddAddrs(peer.ID, peer.Addrs, peerstore.ConnectedAddrTTL)
		}
	}

}

func (p2p *P2PServer) advertiseSelf() {
	routingDiscovery := drouting.NewRoutingDiscovery(p2p.dht)
	routingDiscovery.Advertise(context.Background(), p2p.topicName())
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

func (l *P2PServer) Host() host.Host {
	if l.host == nil {
		panic("Host is not initialized")
	}
	return l.host
}
