package threadsafe

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrFullChannel    = errors.New("channel full")
	ErrLockedChannel  = errors.New("channel locked")
	ErrContextTimeout = errors.New("context timed out.")
)

type LockedConsumer[T any] struct {
	lock      bool
	lockRwMtx *sync.RWMutex
	c         chan T
}

func NewLockedConsumer[T any](cap int) *LockedConsumer[T] {
	return &LockedConsumer[T]{
		lock:      false,
		lockRwMtx: &sync.RWMutex{},
		c:         make(chan T, cap),
	}
}

func (l *LockedConsumer[T]) Lock() {
	l.lockRwMtx.Lock()
	defer l.lockRwMtx.Unlock()
	l.lock = true
}

func (l *LockedConsumer[T]) Unlock() {
	l.lockRwMtx.Lock()
	defer l.lockRwMtx.Unlock()
	l.lock = false
}

// returning an early return signal - a condition to keep collecting data
type CollectFunc[T any] func(T) bool

// consume data until consumeFunc returns true within the context window,
// otherwise ErrContextTimeout
func (l *LockedConsumer[T]) Collect(
	ctx context.Context,
	consumeFunc CollectFunc[T],
) error {
	l.Unlock()
	defer l.Lock()

	for {
		select {
		case <-ctx.Done():
			return ErrContextTimeout

		case b := <-l.c:
			if consumeFunc(b) {
				return nil
			}
		}
	}
}

func (l *LockedConsumer[T]) Consume(v T) error {
	l.lockRwMtx.RLock()
	defer l.lockRwMtx.RUnlock()

	if l.lock {
		return ErrLockedChannel
	}

	select {
	case l.c <- v:
		return nil

	default:
		return ErrFullChannel
	}
}
