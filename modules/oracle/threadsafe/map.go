package threadsafe

import (
	"maps"
	"sync"
)

type Map[K comparable, V any] struct {
	buf map[K]V
	mtx *sync.Mutex
}

func NewMap[K comparable, V any]() *Map[K, V] {
	return &Map[K, V]{
		buf: make(map[K]V),
		mtx: &sync.Mutex{},
	}
}

type MapUpdateFunc[K comparable, V any] func(map[K]V)

func (t *Map[K, V]) Update(mapUpdateFunc MapUpdateFunc[K, V]) {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	mapUpdateFunc(t.buf)
}

func (t *Map[K, V]) Insert(k K, v V) {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	t.buf[k] = v
}

func (t *Map[K, V]) Get() map[K]V {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	bufCpy := make(map[K]V)
	maps.Copy(bufCpy, t.buf)

	return bufCpy
}

func (t *Map[K, V]) Clear() {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	t.buf = make(map[K]V)
}
