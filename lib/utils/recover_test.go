package utils

import (
	"errors"
	"sync"
	"testing"
)

// Pentest finding N-L4 — pin the recover semantics so a future
// change that drops the defer trips the test.

func TestNL4_RecoverGoroutineSwallowsPanic(t *testing.T) {
	// If the panic is NOT swallowed it propagates up to the test
	// runner and fails this test (or kills the process). A passing
	// test means the helper caught it.
	done := make(chan struct{})
	go RecoverGoroutine("test.swallow", func() {
		defer close(done)
		panic("simulated handler crash")
	})
	<-done
}

func TestNL4_RecoverGoroutineNonPanicPath(t *testing.T) {
	// fn returns normally — RecoverGoroutine should be transparent.
	called := false
	RecoverGoroutine("test.normal", func() {
		called = true
	})
	if !called {
		t.Fatal("normal path: fn was not invoked")
	}
}

func TestNL4_RecoverGoroutineConcurrentPanics(t *testing.T) {
	// Multiple panicking goroutines must not bleed into each other.
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			RecoverGoroutine("test.concurrent", func() {
				if i%2 == 0 {
					panic(errors.New("even panic"))
				}
				// odd indices return normally
			})
		}()
	}
	wg.Wait()
}
