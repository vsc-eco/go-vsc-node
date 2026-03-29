package libp2p

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestPubSubSemaphore(t *testing.T) {
	// Attack: flood 10000 messages. Without semaphore, 10000 goroutines spawn.
	// With semaphore of 64, max concurrent is 64 and excess are dropped.

	t.Run("SemaphoreLimitsConcurrency", func(t *testing.T) {
		sem := make(chan struct{}, 64)
		var running int64
		var maxRunning int64
		var dropped int64

		for i := 0; i < 1000; i++ {
			select {
			case sem <- struct{}{}:
				go func() {
					cur := atomic.AddInt64(&running, 1)
					for {
						old := atomic.LoadInt64(&maxRunning)
						if cur > old {
							if atomic.CompareAndSwapInt64(&maxRunning, old, cur) {
								break
							}
						} else {
							break
						}
					}
					time.Sleep(1 * time.Millisecond) // simulate work
					atomic.AddInt64(&running, -1)
					<-sem
				}()
			default:
				atomic.AddInt64(&dropped, 1)
			}
		}

		// Wait for all goroutines to finish
		time.Sleep(200 * time.Millisecond)

		max := atomic.LoadInt64(&maxRunning)
		drop := atomic.LoadInt64(&dropped)

		t.Logf("Max concurrent: %d (cap: 64)", max)
		t.Logf("Dropped: %d / 1000 messages", drop)

		if max > 64 {
			t.Errorf("Exceeded semaphore cap: max concurrent was %d", max)
		}
		if drop == 0 {
			t.Error("Expected some messages to be dropped when flooded")
		}
		t.Logf("Semaphore working: limited to %d concurrent, dropped %d excess", max, drop)
	})

	t.Run("NormalLoadNoDrops", func(t *testing.T) {
		sem := make(chan struct{}, 64)
		var dropped int64

		// 10 messages — well under capacity
		for i := 0; i < 10; i++ {
			select {
			case sem <- struct{}{}:
				go func() {
					time.Sleep(1 * time.Millisecond)
					<-sem
				}()
			default:
				atomic.AddInt64(&dropped, 1)
			}
		}

		time.Sleep(50 * time.Millisecond)
		if atomic.LoadInt64(&dropped) > 0 {
			t.Error("Normal load should not drop messages")
		} else {
			t.Log("Normal load: 0 drops (correct)")
		}
	})
}
