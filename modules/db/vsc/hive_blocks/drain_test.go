package hive_blocks

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestListenToBlockUpdatesCancelDrains is the proof for the shutdown
// contract of the returned CancelFunc: canceling while a listener call is
// in flight blocks until that call has returned, and no further listener
// calls are issued after cancel. It runs against a real MongoDB because
// the feed goroutine's exit path depends on server-side cursor/context
// behavior no mock can reproduce.
func TestListenToBlockUpdatesCancelDrains(t *testing.T) {
	uri := startMongo(t, "47020", "vsc-drain-hiveblocks-mongo")
	h := newHiveBlocks(t, uri)

	for n := uint64(1); n <= 10; n++ {
		if err := h.StoreBlocks(10, mkBlock(n)); err != nil {
			t.Fatalf("store %d: %v", n, err)
		}
	}

	var mtx sync.Mutex
	delivered := make([]uint64, 0, 10)
	// inFlightStarted: block 5 entered the listener.
	inFlightStarted := make(chan struct{}, 1)
	// inFlightDone: block 5's listener call returned.
	inFlightDone := make(chan struct{})
	// release holds block 5 mid-listener to simulate a slow in-flight block.
	release := make(chan struct{})

	listenCtx, listenCancel := context.WithCancel(context.Background())
	defer listenCancel()

	cancel, errChan := h.ListenToBlockUpdates(listenCtx, 1, func(b HiveBlock, head *uint64) error {
		if b.BlockNumber == 5 {
			select {
			case inFlightStarted <- struct{}{}:
			default:
			}
			<-release
		}
		mtx.Lock()
		delivered = append(delivered, b.BlockNumber)
		mtx.Unlock()
		if b.BlockNumber == 5 {
			close(inFlightDone)
		}
		return nil
	})
	// Drain errChan so the listener goroutine can never block on it.
	go func() {
		for range errChan {
		}
	}()

	select {
	case <-inFlightStarted:
	case <-time.After(60 * time.Second):
		t.Fatal("block 5 never reached the listener")
	}

	// Cancel while block 5 is mid-listener: the drain contract says this
	// must not return until the listener call has returned.
	cancelReturned := make(chan struct{})
	go func() {
		cancel()
		close(cancelReturned)
	}()

	select {
	case <-cancelReturned:
		mtx.Lock()
		defer mtx.Unlock()
		t.Fatalf("cancel returned while block 5 was still in the listener: %v", delivered)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-cancelReturned:
	case <-time.After(60 * time.Second):
		t.Fatal("cancel did not return after the in-flight listener call finished")
	}

	// The in-flight listener call completed before cancel returned.
	select {
	case <-inFlightDone:
	default:
		t.Fatal("in-flight listener call had not returned when cancel returned")
	}

	// Feed quiesced: the in-flight block and everything before it, nothing after.
	mtx.Lock()
	defer mtx.Unlock()
	for _, n := range delivered {
		if n > 5 {
			t.Fatalf("listener delivered block %d after cancel: %v", n, delivered)
		}
	}
	if len(delivered) != 5 {
		t.Fatalf("listener delivered %v, want blocks 1..5 only", delivered)
	}
}
