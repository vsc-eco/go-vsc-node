package threadsafe

import (
	"context"
	"sync"
	"time"
)

type Slice[T any] struct {
	buf    []T
	bufMtx *sync.Mutex

	lock      bool
	lockRwMtx *sync.RWMutex
}

func NewSlice[T any](cap int) *Slice[T] {
	return &Slice[T]{
		buf:       make([]T, 0, cap),
		bufMtx:    &sync.Mutex{},
		lock:      true,
		lockRwMtx: &sync.RWMutex{},
	}
}

func (t *Slice[T]) Lock() {
	t.lockRwMtx.Lock()
	defer t.lockRwMtx.Unlock()
	t.lock = true
}

func (t *Slice[T]) Unlock() {
	t.lockRwMtx.Lock()
	defer t.lockRwMtx.Unlock()
	t.lock = false
}

func (t *Slice[T]) UnlockTimeout(dur time.Duration) {
	t.Unlock()
	defer t.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	<-ctx.Done()
}

func (t *Slice[T]) Append(v T) bool {
	t.lockRwMtx.RLock()
	defer t.lockRwMtx.RUnlock()

	if t.lock {
		return false
	}

	t.bufMtx.Lock()
	defer t.bufMtx.Unlock()

	t.buf = append(t.buf, v)

	return true
}

func (t *Slice[T]) Slice() []T {
	t.bufMtx.Lock()
	defer t.bufMtx.Unlock()

	bufCpy := make([]T, len(t.buf))
	copy(bufCpy, t.buf)

	return bufCpy
}

func (t *Slice[T]) Clear() {
	t.bufMtx.Lock()
	defer t.bufMtx.Unlock()

	t.buf = t.buf[:0]
}
