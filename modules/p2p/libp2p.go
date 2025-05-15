package libp2p

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/witnesses"
	start_status "vsc-node/modules/start-status"

	"github.com/chebyrash/promise"
	libp2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/routing"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	"github.com/robfig/cron/v3"

	rpc "github.com/libp2p/go-libp2p-gorpc"
	// p "vsc-node/lib/pubsub"
	// "vsc-node/modules/aggregate"

	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
)

var BOOTSTRAP = []string{
	"/dnsaddr/api.vsc.eco/tcp/10720/p2p/12D3KooWFVVQ2xG6ohJ3tQrh3V6zZnRnKPVfQWVH5LjJakzBCs7E",
	"/ip4/149.56.25.168/tcp/10720/p2p/12D3KooWFVVQ2xG6ohJ3tQrh3V6zZnRnKPVfQWVH5LjJakzBCs7E",  // TODO this is api.vsc.eco, but DNS resolution doesn't work?
	"/ip4/173.211.12.65/tcp/10720/p2p/12D3KooWGpWrBc5pFx5GHWibczTPrazDCfk8GCETB5Ynb4Dq5L5V",  //@vaultec.vsc
	"/ip4/147.135.15.155/tcp/10720/p2p/12D3KooWCAE4XrkE4NJL3nqYkXXNhte94rdBDGGVQQJewrDXDVJZ", // mengao
	// "/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	// "/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	// "/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	// "/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	// "/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",         // mars.i.ipfs.io
	// "/ip4/104.131.131.82/udp/4001/quic-v1/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ", // mars.i.ipfs.io
}

type P2PServer struct {
	witnessDb witnesses.Witnesses
	conf      common.IdentityConfig

	Host           host.Host
	Dht            *kadDht.IpfsDHT
	rpcClient      *rpc.Client
	pubsub         *pubsub.PubSub
	multicastTopic *pubsub.Topic
	cron           *cron.Cron

	topics map[string]*pubsub.Topic

	subs    []*pubsub.Subscription
	tickers []*time.Ticker

	startStatus start_status.StartStatus

	port int
}

// var _ aggregate.Plugin = &Libp2p{}
// var _ p.PubSub[peer.ID] = &Libp2p{}
var _ start_status.Starter = &P2PServer{}

func New(witnessDb witnesses.Witnesses, conf common.IdentityConfig, port ...int) *P2PServer {

	p := 10720
	if len(port) > 0 {
		p = port[0]
	}

	return &P2PServer{
		witnessDb:   witnessDb,
		conf:        conf,
		cron:        cron.New(),
		startStatus: start_status.New(),
		port:        p,
	}
}

var topicNameFlag = "/vsc/mainnet/multicast"

// Finds VSC peers through DHT
func bootstrapVSCPeers(ctx context.Context, p2p *P2PServer) {
	h := p2p.Host

	routingDiscovery := drouting.NewRoutingDiscovery(p2p.Dht)
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
	p2pServer.Host = routedHost
	p2pServer.Dht = idht
	fmt.Println("peer ID:", p2pServer.PeerInfo().GetPeerId())

	//Setup GORPC server and client
	var protocolID = protocol.ID("/vsc.network/rpc")
	rpcServer := rpc.NewServer(routedHost, protocolID)
	rpcClient := rpc.NewClientWithServer(routedHost, protocolID, rpcServer)

	go func() {
		cSub, _ := p2pServer.Host.EventBus().Subscribe(new(event.EvtLocalReachabilityChanged))
		defer cSub.Close()

		select {
		case stat := <-cSub.Out():

			fmt.Println("NAT Status", stat)
		case <-time.After(30 * time.Second):

		}
	}()
	svc := &RPCService{
		p2pService: p2pServer,
	}

	//Register associated services. It can be more than one, name must be unique
	rpcServer.RegisterName("witness", svc)
	p2pServer.rpcClient = rpcClient

	//Setup pubsub
	ps, err := pubsub.NewGossipSub(ctx, p2p, pubsub.WithDiscovery(drouting.NewRoutingDiscovery(p2pServer.Dht)), pubsub.WithPeerExchange(true))
	if err != nil {
		return err
	}

	p2pServer.pubsub = ps

	topic, _ := ps.Join("/vsc/mainnet/multicast")
	topic.Relay()

	ps.RegisterTopicValidator("/vsc/mainnet/multicast", func(ctx context.Context, p peer.ID, msg *pubsub.Message) bool { return true })

	p2pServer.multicastTopic = topic

	// reply := HelloReply{}
	// err := rpcClient.Call(routedHost.ID(), "witness", "HelloWorld", HelloArgs{
	// 	Msg: "hello world",
	// }, reply)

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

	ticker := time.NewTicker(5 * time.Second)
	p := promise.New(func(resolve func(any), reject func(error)) {
		for {
			select {
			case <-ticker.C:
				// do stuff
				// peers := p2ps.host.Network().Peers()
				pubsubPeers := p2ps.multicastTopic.ListPeers()
				for _, val := range pubsubPeers {
					protocols, _ := p2ps.Host.Network().Peerstore().GetProtocols(val)
					for _, protoName := range protocols {
						if protoName == "/vsc.network/rpc" {
							//Do connection stuff
						}
					}
				}
			}
		}
	})
	p2ps.cron.AddFunc("@every 5m", func() {
		p2ps.connectRegisteredPeers()
	})

	p2ps.cron.AddFunc("@every 5m", func() {
		p2ps.discoverPeers()
	})

	uniquePeers := make(map[string]struct{})

	for _, peerStr := range BOOTSTRAP {
		peerId, _ := peer.AddrInfoFromString(peerStr)
		uniquePeers[peerId.ID.String()] = struct{}{}
		p2ps.Host.Connect(context.Background(), *peerId)
	}

	p2ps.Dht.Bootstrap(context.Background())
	go bootstrapVSCPeers(context.Background(), p2ps)
	//First startup to try and get connected to the network
	go p2ps.connectRegisteredPeers()

	p2ps.tickers = append(p2ps.tickers, ticker)

	subscription, _ := p2ps.multicastTopic.Subscribe()

	p2ps.subs = append(p2ps.subs, subscription)

	go func() {
		time.Sleep(50 * time.Millisecond)
		for {
			peerList := ""
			if len(p2ps.Host.Network().Peers()) > 5 {
				for idx, peer := range p2ps.Host.Network().Peers() {
					if idx >= 4 {
						break
					}
					if idx > 0 {
						peerList += " "
					}
					peerList += peer.String()
				}
				peerList += "..." + strconv.Itoa(len(p2ps.Host.Network().Peers())-4) + " more"
			} else {
				for _, peer := range p2ps.Host.Network().Peers() {
					peerList += peer.String() + ", "
				}
			}
			peerLen := len(p2ps.Host.Network().Peers())
			fmt.Println("peers", "["+peerList+"]", "peers.len()="+strconv.Itoa(peerLen))
			if peerLen >= len(uniquePeers)-1 {
				p2ps.startStatus.TriggerStart()
			}

			time.Sleep(5 * time.Second)
		}
	}()

	return p
}

// Stop implements aggregate.Plugin.
func (p2p *P2PServer) Stop() error {

	//Clean up remaining pubsub subscriptions
	for _, value := range p2p.subs {
		value.Cancel()
	}

	for _, value := range p2p.tickers {
		value.Stop()
	}

	return nil
}

func (p2p *P2PServer) PeerInfo() common.PeerInfoGetter {
	return &peerGetter{
		server: p2p,
	}
}

func (p2p *P2PServer) connectRegisteredPeers() {
	witnesses, _ := p2p.witnessDb.GetLastestWitnesses()

	for _, witness := range witnesses {
		if witness.PeerId == "" {
			continue
		}
		peerId, _ := peer.AddrInfoFromString("/p2p/" + witness.PeerId)

		for _, peer := range p2p.Host.Network().Peers() {
			if peer.String() == peerId.ID.String() {
				p2p.Host.Connect(context.Background(), *peerId)
			}
		}
		p2p.Host.Connect(context.Background(), *peerId)
	}
}

func (p2p *P2PServer) discoverPeers() {

	if len(p2p.Host.Network().Peers()) < 2 {
		for _, peerStr := range BOOTSTRAP {
			peerId, _ := peer.AddrInfoFromString(peerStr)

			p2p.Host.Connect(context.Background(), *peerId)
		}
	}

	ctx := context.Background()

	h := p2p.Host

	routingDiscovery := drouting.NewRoutingDiscovery(p2p.Dht)
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
