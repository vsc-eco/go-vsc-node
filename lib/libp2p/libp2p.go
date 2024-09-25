package libp2p

import (
	"context"
	"errors"
	"fmt"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	ping "github.com/libp2p/go-libp2p/p2p/protocol/ping"

	rpc "github.com/libp2p/go-libp2p-gorpc"
	// p "vsc-node/lib/pubsub"
	// "vsc-node/modules/aggregate"
)

var BOOTSTRAP = []string{
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	"/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",         // mars.i.ipfs.io
	"/ip4/104.131.131.82/udp/4001/quic-v1/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ", // mars.i.ipfs.io
}

type P2PServer struct {
	host           host.Host
	rpcClient      *rpc.Client
	pubsub         *pubsub.PubSub
	multicastTopic *pubsub.Topic

	subs    []*pubsub.Subscription
	tickers []*time.Ticker
}

// var _ aggregate.Plugin = &Libp2p{}
// var _ p.PubSub[peer.ID] = &Libp2p{}

func New() *P2PServer {

	return &P2PServer{}
}

// =================================
// ===== Plugin Implementation =====
// =================================

// Init implements aggregate.Plugin.
func (p2pServer *P2PServer) Init() error {
	//Future initialize using a configuration object with more detailed info
	p2p, _ := libp2p.New(libp2p.Identity(nil))

	//DHT wrapped host
	ctx := context.Background()
	dht, _ := kadDht.New(ctx, p2p)
	routedHost := rhost.Wrap(p2p, dht)
	p2pServer.host = routedHost

	fmt.Println("Starting up")

	//Setup GORPC server and client
	var protocolID = protocol.ID("/vsc.network/rpc")
	rpcServer := rpc.NewServer(routedHost, protocolID)
	rpcClient := rpc.NewClientWithServer(routedHost, protocolID, rpcServer)

	svc := &RPCService{
		p2pService: p2pServer,
	}

	//Register associated services. It can be more than one, name must be unique
	rpcServer.RegisterName("witness", svc)
	p2pServer.rpcClient = rpcClient

	//Setup pubsub
	ps, _ := pubsub.NewGossipSub(ctx, p2p)

	p2pServer.pubsub = ps

	topic, _ := ps.Join("/vsc/mainnet/multicast")
	p2pServer.multicastTopic = topic

	// reply := HelloReply{}
	// err := rpcClient.Call(routedHost.ID(), "witness", "HelloWorld", HelloArgs{
	// 	Msg: "hello world",
	// }, reply)

	return nil
}

// Start implements aggregate.Plugin.
func (p2ps *P2PServer) Start() error {
	//What would we "start" for P2P?

	//Ask for P2P profiling from other nodes

	// send := make(chan HelloArgs)
	// reply := make(chan HelloReply)
	// ctx := context.Background()

	// err := p2ps.rpcClient.Stream(ctx, p2ps.host.ID(), "witness", "HelloWorld", send, reply)

	// fmt.Println("error is", err)

	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				// do stuff
				fmt.Println("it's ticking yeah!")
			}
		}
	}()
	p2ps.tickers = append(p2ps.tickers, ticker)
	// ticker.Stop()

	for _, peerStr := range BOOTSTRAP {
		peerId, _ := peer.AddrInfoFromString(peerStr)

		p2ps.host.Connect(context.Background(), *peerId)
	}
	subscription, _ := p2ps.multicastTopic.Subscribe()

	p2ps.handleMulticast(subscription)

	p2ps.subs = append(p2ps.subs, subscription)

	// peerId, _ := peer.AddrInfoFromString("/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWAvxZcLJmZVUaoAtey28REvaBwxvfTvQfxWtXJ2fpqWnw")
	// connectErr := p2ps.host.Connect(ctx, *peerId)

	return nil
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

func (l *P2PServer) handleMulticast(subscription *pubsub.Subscription) error {
	ctx := context.Context(context.Background())
	go func() {
		for {
			msg, _ := subscription.Next(ctx)

			fmt.Println(msg.GetFrom(), string(msg.GetData()))
		}
	}()

	return nil
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

func main() {
	p2p, _ := libp2p.New()

	fmt.Println(p2p.ID())
	ctx := context.Background()
	dht, _ := kadDht.New(ctx, p2p)
	routedHost := rhost.Wrap(p2p, dht)

	fmt.Println(routedHost)

	peerId, _ := peer.AddrInfoFromString("/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWAvxZcLJmZVUaoAtey28REvaBwxvfTvQfxWtXJ2fpqWnw")
	connectErr := p2p.Connect(ctx, *peerId)
	peerId, _ = peer.AddrInfoFromString("/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ")
	connectErr = p2p.Connect(ctx, *peerId)

	const test string = "Hello, World!"

	fmt.Println(test)

	node2, _ := libp2p.New()
	fmt.Println()
	var addr = p2p.Addrs()[0].String() + "/p2p/" + p2p.ID().String()

	addrInfo, _ := peer.AddrInfoFromString(addr)
	node2.Connect(ctx, *addrInfo)

	// p2p.SetStreamHandler("/vsc.eco/multicast/1.0.0")
	// str, err := node2.NewStream(ctx, p2p.ID(), "/vsc.eco/multicast/1.0.0")

	// if err != nil {
	// 	fmt.Println(err)
	// } else {

	// 	str.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	// 	fmt.Println(str.Conn().Stat())
	// }

	dht.Bootstrap((ctx))

	// peerInfo,_ := dht.FindPeer(ctx, peer.ID("12D3KooWLxp3mk99i9QYt1wNzGzv1zLS1ZppofTkw3bEgz9FwvS4"))

	// fmt.Println(peerInfo)
	fmt.Println(p2p.Network().Peers())
	fmt.Println(connectErr)

	ps, _ := pubsub.NewGossipSub(ctx, p2p)

	s, _ := p2p.NewStream(context.TODO(), peerId.ID, "vsc-ksdljfl")
	_ = s

	ping.NewPingService(p2p)
	pctx, cancel := context.WithCancel(context.Background())
	pingChan := ping.Ping(pctx, p2p, peerId.ID)
	for val := range pingChan {
		fmt.Println(val.RTT.Milliseconds())
		cancel()
	}

	// ps.Publish("topic", []byte("Hello, World!"))

	// p2p.

	topic, _ := ps.Join("test-topic")

	subscription, _ := topic.Subscribe()

	go func() {
		for {
			msg, _ := subscription.Next(ctx)

			fmt.Println(msg.GetFrom(), string(msg.GetData()))
		}
	}()

	time.Sleep(5 * time.Second)
	topic.Publish(ctx, []byte("Hello, World!"))

	go func() {
		for {
			time.Sleep(5 * time.Second)
			fmt.Println("Connected Peers", len(routedHost.Network().Peers()))

			//dht.Bootstrap(ctx)
			pi, err := dht.NetworkSize()
			fmt.Println("NetworkSize", pi, err)
			// peers,_ := dht.FindProviders(ctx, cid.Cid)

			// fmt.Println(peers)
		}
	}()

	for {
		time.Sleep(2500 * time.Second)
	}

	//ptr := bls12381.NewG1().One()

	// ptr,err1 := bls.HashToG1([]byte("Hello, World!"), []byte("dstdd"))
	// ptr2,_ := bls.HashToG1([]byte("Hello, World!"), []byte("dstdd"))

	// if err1 == nil {

	// 	fmt.Println(ptr.Y)
	// 	fmt.Println(ptr.Add(&ptr, &ptr2).Y)
	// 	println("Hello, World!")
	// } else {
	// 	println("Error", err1.Error())
	// }
}
