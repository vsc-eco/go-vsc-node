package p2p

import (
	"context"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
)

func TestP2PSpec(t *testing.T) {
	t.Run("topic", func(t *testing.T) {
		srv := &p2pSpec{}
		assert.Equal(t, oracleTopic, srv.Topic())
	})

	t.Run("p2pSpec.HandleMessage", testHandleMessage)
}

func testHandleMessage(t *testing.T) {
	c := make(chan []ObservePricePoint, 10)
	srv := &p2pSpec{
		observePriceChan: c,
	}

	t.Run("observed prices forwarding", func(t *testing.T) {
		data := make([]ObservePricePoint, 2)
		msg := OracleMessage{
			Type: MsgOraclePriceObserve,
			Data: data,
		}

		assert.NoError(
			t,
			srv.HandleMessage(
				context.Background(), peer.ID(""),
				&msg, stubSend,
			),
		)

		assert.Equal(t, len(data), len(<-c))
	})
}

func stubSend(msg Msg) error {
	return nil
}
