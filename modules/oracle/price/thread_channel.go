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

type PriceChannel = threadChan[map[string]api.PricePoint]
type SignatureRequestChannel = threadChan[SignatureRequestMessage]
type SignatureResponseChannel = threadChan[SignatureResponseMessage]

type threadChan[T any] struct {
	channel chan T
	rwLock  *sync.RWMutex
}

func MakeThreadChan[T any]() *threadChan[T] {
	return &threadChan[T]{
		channel: nil,
		rwLock:  &sync.RWMutex{},
	}
}

func (t *threadChan[T]) Open() <-chan T {
	t.rwLock.Lock()
	defer t.rwLock.Unlock()

	t.channel = make(chan T, 128)

	return t.channel
}

func (t *threadChan[T]) Close() error {
	t.rwLock.Lock()
	defer t.rwLock.Unlock()

	if t.channel == nil {
		return errNilChannel
	}

	close(t.channel)
	t.channel = nil

	return nil
}

func (t *threadChan[T]) Send(data T) error {
	t.rwLock.RLock()
	defer t.rwLock.RUnlock()

	if t.channel == nil {
		return errNilChannel
	}

	select {
	case t.channel <- data:
		return nil

	default:
		return errFullChannel
	}
}
