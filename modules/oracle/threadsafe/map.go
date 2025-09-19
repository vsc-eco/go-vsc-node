package threadsafe

import (
	"context"
	"maps"
	"sync"
	"time"
)

type Map[K comparable, V any] struct {
	*sync.Mutex
	buf map[K]V
}

func NewMap[K comparable, V any]() *Map[K, V] {
	return &Map[K, V]{
		Mutex: &sync.Mutex{},
		buf:   make(map[K]V),
	}
}

// unlock the mutex
func (t *Map[K, V]) UnlockTimeout(dur time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	t.Unlock()
	<-ctx.Done()
	t.Lock()
}

type MapUpdateFunc[K comparable, V any] func(map[K]V)

func (t *Map[K, V]) Update(
	threadBlocking bool,
	mapUpdateFunc MapUpdateFunc[K, V],
) bool {
	if threadBlocking {
		t.Lock()
	} else if !t.TryLock() {
		return false
	}

	defer t.Unlock()

	mapUpdateFunc(t.buf)

	return true
}

func (t *Map[K, V]) Insert(threadBlocking bool, k K, v V) bool {
	if threadBlocking {
		t.Lock()
	} else if !t.TryLock() {
		return false
	}

	defer t.Unlock()

	t.buf[k] = v

	return true
}

// returns a deep copy of the internal map
// not thread safe
func (t *Map[K, V]) Get() map[K]V {
	bufCpy := make(map[K]V)
	maps.Copy(bufCpy, t.buf)

	return bufCpy
}

// not thread safe
func (t *Map[K, V]) Clear() {
	t.buf = make(map[K]V)
}
