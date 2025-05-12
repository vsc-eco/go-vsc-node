package libp2p_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chebyrash/promise"
	libp2p "github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	"github.com/stretchr/testify/assert"
)

const BYTES_IN_INT32 = 4

// func UnsafeCaseInt32ToBytes(val int32) []byte {
// 	hdr := reflect.SliceHeader{Data: uintptr(unsafe.Pointer(&val)), Len: BYTES_IN_INT32, Cap: BYTES_IN_INT32}
// 	return *(*[]byte)(unsafe.Pointer(&hdr))
// }

// func UnsafeCaseBytesToInt32(val []byte) int32 {
// 	return (*(*[]int32)(unsafe.Pointer(&val)))[0]
// }

var sentMsgs = atomic.Int32{}
var recvMsgs = atomic.Int32{}
var validMsgs = atomic.Int32{}

const nodeCount = 6

func Test(t *testing.T) {
	type Runner struct {
		host.Host
		run func()
	}
	runners := make([]Runner, nodeCount)

	for node := range nodeCount {
		h, r := runner(t)
		runners[node] = Runner{
			h,
			r,
		}
	}

	for i, r1 := range runners {
		for j := range nodeCount - i - 1 {
			r2 := runners[i+j+1]
			connect(t, r1.Host, r2.Host)
		}
	}

	for _, runner := range runners {
		go runner.run()
	}

	assert.Eventually(t, func() bool {
		return sentMsgs.Load() == int32(nodeCount)*2
	}, 50*time.Second, time.Millisecond)

	promise.All(
		context.Background(),
		// promise.New(func(resolve func(any), reject func(error)) {
		// 	assert.Eventually(t, func() bool {
		// 		return recvMsgs.Load() == sentMsgs.Load()*int32(nodeCount)
		// 	}, 50*time.Second, time.Millisecond)
		// }),
		promise.New(func(resolve func(any), reject func(error)) {
			assert.Eventually(t, func() bool {
				return validMsgs.Load() == sentMsgs.Load()*int32(nodeCount)
			}, 50*time.Second, time.Millisecond)
		}),
	).Await(context.Background())
}

func connect(t *testing.T, h1, h2 host.Host) {
	err := h1.Connect(context.Background(), peer.AddrInfo{
		ID:    h2.ID(),
		Addrs: h2.Addrs(),
	})
	assert.NoError(t, err)

	Ping(t, h1, h2)
}

func Ping(t *testing.T, h1, h2 host.Host) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := <-ping.Ping(ctx, h1, h2.ID())
	fmt.Println(h1.ID().String(), "pings", h2.ID().String(), res.RTT.String())
	assert.NoError(t, res.Error)
	res = <-ping.Ping(ctx, h2, h1.ID())
	fmt.Println(h2.ID().String(), "pings", h1.ID().String(), res.RTT.String())
	assert.NoError(t, res.Error)
}

func runner(t *testing.T) (host.Host, func()) {
	p2p, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"))
	assert.NoError(t, err)

	//DHT wrapped host
	ctx := context.Background()

	//Setup pubsub
	ps, err := pubsub.NewGossipSub(ctx, p2p)
	assert.NoError(t, err)

	run := func() {

		topic, err := ps.Join("topic-1")
		assert.NoError(t, err)

		_, err = topic.Relay()
		assert.NoError(t, err)

		// ps.RegisterTopicValidator("topic-1", func(ctx context.Context, p peer.ID, msg *pubsub.Message) bool {
		// 	recvMsgs.Add(1)
		// 	return true
		// })

		send := func() {
			msgId := sentMsgs.Add(1)
			err := topic.Publish(ctx, []byte{byte(msgId)})
			assert.NoError(t, err)
		}

		sub, _ := topic.Subscribe()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
					msg, err := sub.Next(ctx)

					fmt.Println("msg", msg.ReceivedFrom.String(), p2p.ID().String())

					assert.NoError(t, err)
					if err == nil {
						validMsgs.Add(1)
					}
				}
			}
		}()

		assert.Eventually(t, func() bool {
			fmt.Println(topic.ListPeers())
			return len(topic.ListPeers()) == nodeCount-1
		}, 3*time.Second, 10*time.Millisecond)

		send()

		time.Sleep(time.Second)

		send()
	}

	return p2p, run
}
