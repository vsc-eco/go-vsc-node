package threadsafe

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrLockedChannel is returned by Consume while the consumer is locked,
	// meaning it is not currently accepting new items.
	ErrLockedChannel = errors.New("locked channel")

	errConsumerFull = errors.New("consumer channel full")
)

// CollectFunc processes a single collected item and returns true to stop
// collecting early.
type CollectFunc[T any] func(T) bool

// LockedConsumer is a buffered channel that rejects new items via Consume
// while locked. Collect temporarily unlocks it, drains buffered items as well
// as any items arriving during the collection window, then re-locks before
// returning.
type LockedConsumer[T any] struct {
	mu     sync.Mutex
	locked bool
	ch     chan T
}

func NewLockedConsumer[T any](capacity int) *LockedConsumer[T] {
	return &LockedConsumer[T]{
		ch: make(chan T, capacity),
	}
}

// Lock prevents Consume from accepting new items.
func (c *LockedConsumer[T]) Lock() {
	c.setLocked(true)
}

func (c *LockedConsumer[T]) setLocked(locked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.locked = locked
}

// Consume buffers an item unless the consumer is locked or the buffer is full.
func (c *LockedConsumer[T]) Consume(data T) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.locked {
		return ErrLockedChannel
	}

	select {
	case c.ch <- data:
		return nil
	default:
		return errConsumerFull
	}
}

// Collect unlocks the consumer and feeds items to fn until fn returns true,
// the context is done, or the channel is closed. It returns ctx.Err() when
// the context expires.
func (c *LockedConsumer[T]) Collect(ctx context.Context, fn CollectFunc[T]) error {
	c.setLocked(false)
	defer c.setLocked(true)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case data, ok := <-c.ch:
			if !ok {
				return nil
			}
			if fn(data) {
				return nil
			}
		}
	}
}

// Map is a thread-safe wrapper around a Go map. Get returns a snapshot copy.
type Map[K comparable, V any] struct {
	mu  sync.RWMutex
	buf map[K]V
}

func NewMap[K comparable, V any]() *Map[K, V] {
	return &Map[K, V]{
		buf: make(map[K]V),
	}
}

// Get returns a copy of the underlying map.
func (m *Map[K, V]) Get() map[K]V {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[K]V, len(m.buf))
	for k, v := range m.buf {
		out[k] = v
	}
	return out
}

// Update atomically mutates the underlying map inside fn.
func (m *Map[K, V]) Update(fn func(map[K]V)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m.buf)
}

// Clear removes all entries from the map.
func (m *Map[K, V]) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.buf)
}
