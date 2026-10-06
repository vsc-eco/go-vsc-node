package haf

import (
	"context"
	"sync"
	"testing"
	"time"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"github.com/stretchr/testify/assert"
)

// TestListenToBlockUpdatesCancelDrains is the proof for the shutdown
// contract of Source.ListenToBlockUpdates' CancelFunc: canceling while a
// listener call is in flight blocks until that call has returned, and no
// further listener calls are issued after cancel.
func TestListenToBlockUpdatesCancelDrains(t *testing.T) {
	headers := make([]BlockRow, 0, 5)
	for n := uint64(1); n <= 5; n++ {
		headers = append(headers, header(n))
	}
	f := &stubFetcher{headers: headers, head: 5}
	store := &test_utils.MockHiveBlockDb{}
	src := newSource(f, store, []streamer.FilterFunc{vscFilter}, nil, 0)

	var mtx sync.Mutex
	delivered := make([]uint64, 0, 5)
	// inFlightStarted: block 3 entered the listener.
	inFlightStarted := make(chan struct{}, 1)
	// inFlightDone: block 3's listener call returned.
	inFlightDone := make(chan struct{})
	// release holds block 3 mid-listener to simulate a slow in-flight block.
	release := make(chan struct{})

	listenCtx, listenCancel := context.WithCancel(context.Background())
	defer listenCancel()

	cancel, errs := src.ListenToBlockUpdates(listenCtx, 1, func(b hive_blocks.HiveBlock, head *uint64) error {
		if b.BlockNumber == 3 {
			select {
			case inFlightStarted <- struct{}{}:
			default:
			}
			<-release
		}
		mtx.Lock()
		delivered = append(delivered, b.BlockNumber)
		mtx.Unlock()
		if b.BlockNumber == 3 {
			close(inFlightDone)
		}
		return nil
	})

	select {
	case <-inFlightStarted:
	case err := <-errs:
		t.Fatalf("listener failed before reaching block 3: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("block 3 never reached the listener")
	}

	// Cancel while block 3 is mid-listener: the drain contract says this
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
		t.Fatalf("cancel returned while block 3 was still in the listener: %v", delivered)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-cancelReturned:
	case <-time.After(5 * time.Second):
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
	assert.Equal(t, []uint64{1, 2, 3}, delivered)
}
