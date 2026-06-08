package chain

// review8 GV-H6 — recentlyWitnessed was an unsynchronized map written by the
// p2p message-handler goroutine (witnessChainData) and read/deleted by the
// block-tick goroutine (handleBlockTick). Concurrent access is a
// `fatal error: concurrent map writes` that kills the process (recover cannot
// catch it). The accessors are now mutex-guarded.
//
// Run with -race: this hammers all three access paths concurrently. It is
// race-clean with the mutex; remove the locking in markWitnessed/witnessedAt/
// clearWitnessed and the race detector flags it (and without -race it can crash
// with concurrent map writes).

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestReview8_GVH6_RecentlyWitnessedConcurrentAccess(t *testing.T) {
	o := &ChainOracle{recentlyWitnessed: make(map[string]time.Time)}

	const goroutines = 8
	const iters = 3000

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				key := fmt.Sprintf("BTC:%d-%d", g%2, i%16)
				switch i % 3 {
				case 0:
					o.markWitnessed(key)
				case 1:
					_, _ = o.witnessedAt(key)
				default:
					o.clearWitnessed(key)
				}
			}
		}(g)
	}
	wg.Wait()
}
