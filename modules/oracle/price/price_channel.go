package price

import (
	"errors"
	"sync"
	"vsc-node/modules/oracle/price/api"
)

var (
	errFullChannel = errors.New("price channel full")
	errNilChannel  = errors.New("price channel not initialized")
)

type PriceChannel = threadChan[string, api.PricePoint]

type threadChan[K comparable, V any] struct {
	channel chan map[K]V
	rwLock  *sync.RWMutex
}

func makePriceChannel() *PriceChannel {
	return &threadChan[string, api.PricePoint]{
		channel: nil,
		rwLock:  &sync.RWMutex{},
	}
}

func (p *threadChan[K, V]) Open() <-chan map[K]V {
	p.rwLock.Lock()
	defer p.rwLock.Unlock()

	p.channel = make(chan map[K]V, 128)
	return p.channel
}

func (p *threadChan[K, V]) Close() {
	p.rwLock.Lock()
	defer p.rwLock.Unlock()

	close(p.channel)
	p.channel = nil
}

func (p *threadChan[K, V]) Receive(data map[K]V) error {
	p.rwLock.RLock()
	defer p.rwLock.RUnlock()

	if p.channel == nil {
		return errNilChannel
	}

	select {
	case p.channel <- data:
		return nil

	default:
		return errFullChannel
	}
}
