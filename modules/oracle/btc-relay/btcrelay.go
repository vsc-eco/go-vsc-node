package btcrelay

import (
	"context"
	"fmt"
	"time"
)

type (
	BtcChainRelay struct {
		c chan BtcChainMessage
	}

	BtcChainMessage struct{}
)

func (b *BtcChainRelay) Chan() <-chan BtcChainMessage {
	return b.c
}

func New() BtcChainRelay {
	return BtcChainRelay{
		c: make(chan BtcChainMessage, 1),
	}
}

func (b *BtcChainRelay) Poll(ctx context.Context, relayInterval time.Duration) {
	ticker := time.NewTicker(relayInterval)

	select {
	case <-ctx.Done():
		return

	case <-ticker.C:
		b.fetchChain()
	}
}

func (b *BtcChainRelay) fetchChain() {
	fmt.Println("TODO: implement BtcChainRelay.fetchChain")
}
