package streamer_test

// ===== NOTE =====
// These tests prove the graceful-shutdown drain contract of
// StreamReader.Stop: a stop issued while a block is mid-processing must not
// return until that block's process function has fully returned, and the
// feed must be quiesced so no later block is ever processed.
// ===== NOTE =====

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

// TestStopWaitsForInFlightBlock is the shutdown-drain proof: Stop() called
// while block 5 is mid-processing blocks until block 5's process function
// returns, checkpoints the drained height, and quiesces the feed.
func TestStopWaitsForInFlightBlock(t *testing.T) {
	mockHiveBlocks := &test_utils.MockHiveBlockDb{}
	seedBlockData(t, mockHiveBlocks, 10)

	var mtx sync.Mutex
	processed := make([]uint64, 0, 10)
	// blockStarted: block 5 entered the process function.
	blockStarted := make(chan struct{}, 1)
	// blockDone: block 5's process function returned.
	blockDone := make(chan struct{})
	// release holds block 5 mid-processing to simulate a slow in-flight block.
	release := make(chan struct{})

	process := func(block hive_blocks.HiveBlock, headHeight *uint64) {
		if block.BlockNumber == 5 {
			select {
			case blockStarted <- struct{}{}:
			default:
			}
			<-release
		}
		mtx.Lock()
		processed = append(processed, block.BlockNumber)
		mtx.Unlock()
		if block.BlockNumber == 5 {
			close(blockDone)
		}
	}

	sr := streamer.NewStreamReader(mockHiveBlocks, process, nil, 0)
	assert.NoError(t, sr.Init())
	startPromise := sr.Start()

	// Wait until block 5 is mid-processing (blocks 1-4 fully processed).
	select {
	case <-blockStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("block 5 never started processing")
	}

	stopReturned := make(chan struct{})
	go func() {
		assert.NoError(t, sr.Stop())
		close(stopReturned)
	}()

	// Stop must NOT return while block 5 is still in flight.
	select {
	case <-stopReturned:
		mtx.Lock()
		defer mtx.Unlock()
		t.Fatalf("Stop returned while block 5 was still in flight; processed=%v", processed)
	case <-time.After(200 * time.Millisecond):
	}

	// Let the in-flight block finish; Stop must now return promptly.
	close(release)
	select {
	case <-stopReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the in-flight block finished processing")
	}

	// The block finished processing before Stop returned.
	select {
	case <-blockDone:
	default:
		t.Fatal("in-flight block had not finished when Stop returned")
	}

	// Start's promise resolves once the drained poll loop exits.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := startPromise.Await(ctx); err != nil {
		t.Fatalf("Start promise did not resolve after the drain: %v", err)
	}

	// The drained height is checkpointed for a minimal-replay restart.
	lp, err := mockHiveBlocks.GetLastProcessedBlock()
	assert.NoError(t, err)
	assert.Equal(t, uint64(5), lp, "final last-processed checkpoint must reflect the drained block")

	// Feed quiesced: the in-flight block and everything before it, nothing after.
	mtx.Lock()
	defer mtx.Unlock()
	for _, n := range processed {
		if n > 5 {
			t.Fatalf("block %d was processed after Stop quiesced the feed: %v", n, processed)
		}
	}
	assert.Equal(t, []uint64{1, 2, 3, 4, 5}, processed)
}

// TestStopBeforeStartDoesNotHang proves Stop is safe when Start never ran:
// it must return immediately instead of waiting out the drain timeout.
func TestStopBeforeStartDoesNotHang(t *testing.T) {
	sr := streamer.NewStreamReader(&test_utils.MockHiveBlockDb{}, nil, nil, 0)

	done := make(chan struct{})
	go func() {
		assert.NoError(t, sr.Stop())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung even though Start was never called")
	}
}
