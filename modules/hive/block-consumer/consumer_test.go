package blockconsumer

import (
	"testing"
	"time"

	"vsc-node/modules/db/vsc/hive_blocks"
)

// An async tick is counted from before its goroutine starts until it returns, so
// the devnet pacemaker can never see a block as fully handled while it runs.
func TestAsyncTickCountedUntilItReturns(t *testing.T) {
	v := New(nil)
	release := make(chan struct{})
	v.RegisterBlockTick("slow-async", func(uint64, *uint64) { <-release }, true)

	v.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: 7}, nil)
	if got := v.AsyncTicksInFlight(); got != 1 {
		t.Fatalf("async tick in flight: got %d, want 1", got)
	}
	if got := v.ProcessedHeight(); got != 7 {
		t.Fatalf("processed height: got %d, want 7", got)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for v.AsyncTicksInFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("async tick returned but is still counted")
		}
		time.Sleep(time.Millisecond)
	}
}

// ProcessedHeight moves only after a block's sync ticks (and state engine) ran.
func TestProcessedHeightStoredAfterSyncTicks(t *testing.T) {
	v := New(nil)
	var seen []uint64
	v.RegisterBlockTick("sync", func(uint64, *uint64) { seen = append(seen, v.ProcessedHeight()) }, false)

	v.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: 1}, nil)
	v.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: 2}, nil)

	if len(seen) != 2 || seen[0] != 0 || seen[1] != 1 {
		t.Fatalf("during each block's sync tick processed height was %v, want [0 1]", seen)
	}
	if got := v.ProcessedHeight(); got != 2 {
		t.Fatalf("processed height after block 2: got %d", got)
	}
}
