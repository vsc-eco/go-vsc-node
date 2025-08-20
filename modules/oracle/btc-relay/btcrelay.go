package btcrelay

import (
	"context"
	"fmt"
	"time"
)

type (
	BtcChainRelay struct {
		ticker *time.Ticker
		c      chan BtcChainMessage
	}

	BtcChainMessage struct{}
)

func (b *BtcChainRelay) Chan() <-chan BtcChainMessage {
	return b.c
}

func New(pollDuration time.Duration) BtcChainRelay {
	return BtcChainRelay{
		ticker: time.NewTicker(pollDuration),
		c:      make(chan BtcChainMessage, 1),
	}
}

func (b *BtcChainRelay) Poll(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-b.ticker.C:
		fmt.Println("TODO: implement chain relaying")
	}
}
