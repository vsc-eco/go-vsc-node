package threadsafe

import (
	"context"
	"maps"
	"sync"
	"time"
)

type Map[K comparable, V any] struct {
	buf    map[K]V
	bufMtx *sync.Mutex

	lock      bool
	lockRwMtx *sync.RWMutex
}

func NewMap[K comparable, V any]() *Map[K, V] {
	return &Map[K, V]{
		buf:       make(map[K]V),
		bufMtx:    &sync.Mutex{},
		lock:      true,
		lockRwMtx: &sync.RWMutex{},
	}
}

func (t *Map[K, V]) UnlockTimeout(dur time.Duration) {
	t.Unlock()
	defer t.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	<-ctx.Done()
}

type MapUpdateFunc[K comparable, V any] func(map[K]V)

func (t *Map[K, V]) Update(mapUpdateFunc MapUpdateFunc[K, V]) bool {
	t.lockRwMtx.RLock()
	defer t.lockRwMtx.RUnlock()

	if t.lock {
		return false
	}

	t.bufMtx.Lock()
	defer t.bufMtx.Unlock()

	mapUpdateFunc(t.buf)

	return true
}

func (t *Map[K, V]) Insert(k K, v V) bool {
	t.lockRwMtx.RLock()
	defer t.lockRwMtx.RUnlock()

	if t.lock {
		return false
	}

	t.bufMtx.Lock()
	defer t.bufMtx.Unlock()

	t.buf[k] = v

	return true
}

func (t *Map[K, V]) Get() map[K]V {
	t.bufMtx.Lock()
	defer t.bufMtx.Unlock()

	bufCpy := make(map[K]V)
	maps.Copy(bufCpy, t.buf)

	return bufCpy
}

func (t *Map[K, V]) Clear() {
	t.bufMtx.Lock()
	defer t.bufMtx.Unlock()

	t.buf = make(map[K]V)
}
func (t *Map[K, V]) Lock() {
	t.lockRwMtx.Lock()
	defer t.lockRwMtx.Unlock()
	t.lock = true
}

func (t *Map[K, V]) Unlock() {
	t.lockRwMtx.Lock()
	defer t.lockRwMtx.Unlock()
	t.lock = false
}
