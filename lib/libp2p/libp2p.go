package libp2p

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	ping "github.com/libp2p/go-libp2p/p2p/protocol/ping"

	p "vsc-node/lib/pubsub"
	"vsc-node/modules/aggregate"
)

type Libp2p struct{}

var _ aggregate.Plugin = &Libp2p{}
var _ p.PubSub[peer.ID] = &Libp2p{}

func New() *Libp2p {
	return &Libp2p{}
}

// =================================
// ===== Plugin Implementation =====
// =================================

// Init implements aggregate.Plugin.
func (l *Libp2p) Init() error {
	p2p, err := libp2p.New(libp2p.Identity(nil))
	_, _ = p2p, err
	return nil
}

// Start implements aggregate.Plugin.
func (l *Libp2p) Start() error {
	panic("unimplemented")
}

// Stop implements aggregate.Plugin.
func (l *Libp2p) Stop() error {
	panic("unimplemented")
}

// =================================
// ===== PubSub Implementation =====
// =================================

// Peers implements pubsub.PubSub.
func (l *Libp2p) Peers() []peer.ID {
	panic("unimplemented")
}

// SendTo implements pubsub.PubSub.
func (l *Libp2p) SendTo(topic string, message []byte, recipients []peer.ID) {
	panic("unimplemented")
}

// SendToAll implements pubsub.PubSub.
func (l *Libp2p) SendToAll(topic string, message []byte) {
	panic("unimplemented")
}

// Subscribe implements pubsub.PubSub.
func (l *Libp2p) Subscribe(topic string, handler func([]byte)) {
	panic("unimplemented")
}

// Unsubscribe implements pubsub.PubSub.
func (l *Libp2p) Unsubscribe(topic string, handler func([]byte)) {
	panic("unimplemented")
}

// ==================================
// ===== Example Implementation =====
// ==================================

// streamHandler is our function to handle any libp2p-net streams that belong
// to our protocol. The streams should contain an HTTP request which we need
// to parse, make on behalf of the original node, and then write the response
// on the stream, before closing it.
func streamHandler(stream network.Stream) {
	// Remember to close the stream when we are done.
	defer stream.Close()

	// Create a new buffered reader, as ReadRequest needs one.
	// The buffered reader reads from our stream, on which we
	// have sent the HTTP request (see ServeHTTP())
	buf := bufio.NewReader(stream)

	// Read the HTTP request from the buffer
	req, err := http.ReadRequest(buf)
	fmt.Println(req)
	if err != nil {
		stream.Reset()
		fmt.Println(err)
		return
	}
	defer req.Body.Close()

	// We need to reset these fields in the request
	// URL as they are not maintained.
	req.URL.Scheme = "http"
	hp := strings.Split(req.Host, ":")
	if len(hp) > 1 && hp[1] == "443" {
		req.URL.Scheme = "https"
	} else {
		req.URL.Scheme = "http"
	}
	req.URL.Host = req.Host

	outreq := new(http.Request)
	*outreq = *req

	// We now make the request
	fmt.Printf("Making request to %s\n", req.URL)
	resp, err := http.DefaultTransport.RoundTrip(outreq)
	if err != nil {
		stream.Reset()
		fmt.Println(err)
		return
	}

	// resp.Write writes whatever response we obtained for our
	// request back to the stream.
	resp.Write(stream)
}

func main() {
	p2p, _ := libp2p.New()

	fmt.Println(p2p.ID())
	ctx := context.Background()
	dht, _ := kadDht.New(ctx, p2p)
	routedHost := rhost.Wrap(p2p, dht)

	fmt.Println(routedHost)

	peerId, _ := peer.AddrInfoFromString("/ip4/10.0.6.195/tcp/4001/p2p/12D3KooWLxp3mk99i9QYt1wNzGzv1zLS1ZppofTkw3bEgz9FwvS4")
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

	p2p.SetStreamHandler("/vsc.eco/multicast/1.0.0", streamHandler)
	str, err := node2.NewStream(ctx, p2p.ID(), "/vsc.eco/multicast/1.0.0")

	if err != nil {
		fmt.Println(err)
	} else {

		str.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
		fmt.Println(str.Conn().Stat())
	}

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
