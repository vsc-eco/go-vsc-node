package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitBlocksWaitsForNBlocksPastTheStart(t *testing.T) {
	var head atomic.Uint64
	head.Store(500)
	var reads atomic.Int64
	read := func() (uint64, error) {
		if reads.Add(1) == 1 {
			return 0, errors.New("rpc down") // a failed first read is retried, not taken as 0
		}
		return head.Load(), nil
	}
	done := make(chan struct{})
	go func() {
		waitBlocks(read, 80, time.Millisecond)
		close(done)
	}()
	for reads.Load() < 2 { // the failed read, then the start read at 500
		time.Sleep(time.Millisecond)
	}
	for h := uint64(501); h < 580; h++ {
		head.Store(h)
		time.Sleep(2 * time.Millisecond)
		select {
		case <-done:
			t.Fatalf("returned at head %d, want 580", h)
		default:
		}
	}
	head.Store(580)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not return at head 580")
	}
}

func TestIntervalBlocksFromEnv(t *testing.T) {
	t.Setenv("FEED_PUBLISHER_INTERVAL_BLOCKS", "")
	if n, err := intervalBlocksFromEnv(); n != 0 || err != nil {
		t.Fatalf("unset: %d, %v", n, err)
	}
	t.Setenv("FEED_PUBLISHER_INTERVAL_BLOCKS", "80")
	if n, err := intervalBlocksFromEnv(); n != 80 || err != nil {
		t.Fatalf("80: %d, %v", n, err)
	}
	for _, bad := range []string{"0", "-1", "eighty"} {
		t.Setenv("FEED_PUBLISHER_INTERVAL_BLOCKS", bad)
		if _, err := intervalBlocksFromEnv(); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
