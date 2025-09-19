package threadsafe

import (
	"context"
	"sync"
	"time"
)

type Slice[T any] struct {
	*sync.Mutex
	buf []T
}

func NewSlice[T any](cap int) *Slice[T] {
	return &Slice[T]{
		Mutex: new(sync.Mutex),
		buf:   make([]T, 0, cap),
	}
}

func (t *Slice[T]) UnlockTimeout(dur time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	t.Unlock()
	<-ctx.Done()
	t.Lock()
}

func (t *Slice[T]) Append(blocking bool, v T) bool {
	if blocking {
		t.Lock()
	} else if !t.TryLock() {
		return false
	}

	defer t.Unlock()

	t.buf = append(t.buf, v)

	return true
}

// not threadsafe read
func (t *Slice[T]) Slice() []T {
	bufCpy := make([]T, len(t.buf))
	copy(bufCpy, t.buf)

	return bufCpy
}

// not threadsafe write
func (t *Slice[T]) Clear() {
	t.buf = t.buf[:0]
}
