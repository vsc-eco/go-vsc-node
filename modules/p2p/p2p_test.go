package libp2p_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
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

func Test(t *testing.T) {
	p2p, _ := libp2p.New(libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"))

	//DHT wrapped host
	ctx := context.Background()

	//Setup pubsub
	ps, _ := pubsub.NewGossipSub(ctx, p2p)

	topic, _ := ps.Join("topic-1")
	topic.Relay()

	firstMsg := true
	sentMsgs := atomic.Int32{}
	recvMsgs := atomic.Int32{}
	validMsgs := atomic.Int32{}
	ps.RegisterTopicValidator("topic-1", func(ctx context.Context, p peer.ID, msg *pubsub.Message) bool {
		recvMsgs.Add(1)
		res := firstMsg
		firstMsg = false
		return res
	})

	send := func() {
		msgId := sentMsgs.Add(1)
		topic.Publish(ctx, []byte{byte(msgId)})
	}

	sub, _ := topic.Subscribe()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_, err := sub.Next(ctx)
				if err == nil {
					validMsgs.Add(1)
				}
			}
		}
	}()

	send()

	assert.Equal(t, sentMsgs.Load(), int32(1))

	assert.Eventually(t, func() bool {
		return recvMsgs.Load() == 1
	}, time.Second, time.Millisecond)

	assert.Eventually(t, func() bool {
		return validMsgs.Load() == 1
	}, time.Second, time.Millisecond)

	send()

	time.Sleep(3 * time.Second)

	assert.Equal(t, sentMsgs.Load(), int32(2))
	assert.Equal(t, recvMsgs.Load(), int32(2))
	assert.Equal(t, validMsgs.Load(), int32(1))
}
