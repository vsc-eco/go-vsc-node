package threadsafe

import (
	"sync"
)

type Slice[T any] struct {
	buf []T
	mtx *sync.Mutex
}

func NewSlice[T any](cap int) *Slice[T] {
	return &Slice[T]{
		buf: make([]T, 0, cap),
		mtx: new(sync.Mutex),
	}
}

func (t *Slice[T]) Append(v T) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	t.buf = append(t.buf, v)
}

func (t *Slice[T]) Slice() []T {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	bufCpy := make([]T, len(t.buf))
	copy(bufCpy, t.buf)

	return bufCpy
}

func (t *Slice[T]) Clear() {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	t.buf = t.buf[:0]
}
