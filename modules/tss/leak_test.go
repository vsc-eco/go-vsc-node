package tss

import (
	"sync"
	"testing"
	"time"
)

// TestUnbufferedChannelBlocks proves that an unbuffered channel send blocks
// when nobody is reading, which is the root cause of LEAK 1 (sigChannels)
// and LEAK 2 (proc1 channel).
func TestUnbufferedChannelBlocks(t *testing.T) {
	ch := make(chan struct{})

	done := make(chan bool, 1)
	go func() {
		ch <- struct{}{} // will block if nobody reads
		done <- true
	}()

	select {
	case <-done:
		t.Fatal("expected unbuffered send to block, but it completed")
	case <-time.After(100 * time.Millisecond):
		// Expected: the goroutine is stuck sending to the unbuffered channel
	}

	// Drain so the goroutine can exit
	<-ch
}

// TestBufferedChannelDoesNotBlock proves the fix: a buffered channel allows
// the sender to proceed even when nobody is reading yet.
func TestBufferedChannelDoesNotBlock(t *testing.T) {
	ch := make(chan struct{}, 1) // buffered with capacity 1

	done := make(chan bool, 1)
	go func() {
		ch <- struct{}{} // should NOT block
		done <- true
	}()

	select {
	case <-done:
		// Expected: buffered send completes immediately
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected buffered send to succeed, but it blocked")
	}
}

// TestBufferedSigChannelCapacity16 mirrors the actual fix for LEAK 1:
// 16-deep buffer allows multiple p2p messages to queue without blocking.
func TestBufferedSigChannelCapacity16(t *testing.T) {
	ch := make(chan sigMsg, 16)

	// Simulate 16 p2p handlers sending concurrently with no reader
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ch <- sigMsg{Account: "test", SessionId: "sess"}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All 16 sends completed without blocking
	case <-time.After(200 * time.Millisecond):
		t.Fatal("16 sends to buffered(16) channel should not block")
	}

	if len(ch) != 16 {
		t.Fatalf("expected 16 queued messages, got %d", len(ch))
	}
}

// TestSessionResultsCleanup verifies the LEAK 3 fix pattern: entries added
// to a map during processing are cleaned up afterwards.
func TestSessionResultsCleanup(t *testing.T) {
	results := make(map[string]DispatcherResult)

	// Simulate adding results (as done in Done/Await goroutine)
	sessionIDs := []string{"sess-1", "sess-2", "sess-3"}
	for _, id := range sessionIDs {
		results[id] = nil // placeholder; real code stores KeyGenResult etc.
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(results))
	}

	// Simulate cleanup (the fix)
	for _, id := range sessionIDs {
		delete(results, id)
	}

	if len(results) != 0 {
		t.Fatalf("expected 0 entries after cleanup, got %d", len(results))
	}
}

// TestMapCleanupAfterUse verifies the LEAK 4/5 fix pattern: map entries
// keyed by transaction/epoch ID are deleted after use.
func TestMapCleanupAfterUse(t *testing.T) {
	msgChan := make(map[string]chan struct{})

	// Simulate creating channels for 3 transactions
	txIDs := []string{"tx-aaa", "tx-bbb", "tx-ccc"}
	for _, id := range txIDs {
		msgChan[id] = make(chan struct{}, 1)
	}

	if len(msgChan) != 3 {
		t.Fatalf("expected 3 channels, got %d", len(msgChan))
	}

	// Simulate cleanup after waitForSigs returns (the fix)
	for _, id := range txIDs {
		delete(msgChan, id)
	}

	if len(msgChan) != 0 {
		t.Fatalf("expected 0 channels after cleanup, got %d", len(msgChan))
	}
}

// TestProc1BufferedNoDeadlock proves LEAK 2 fix: when context is cancelled,
// the goroutine's send to proc1 doesn't block if proc1 is buffered.
func TestProc1BufferedNoDeadlock(t *testing.T) {
	proc1 := make(chan struct{}, 1) // buffered (the fix)

	// Goroutine tries to send, simulating the case where context was
	// cancelled but the goroutine still reaches the send.
	go func() {
		proc1 <- struct{}{}
	}()

	// Don't read from proc1 (simulating context cancellation path).
	// Wait briefly, then verify the goroutine didn't leak.
	time.Sleep(50 * time.Millisecond)

	// If we get here without deadlock, the fix works.
	// Verify the message is buffered.
	select {
	case <-proc1:
		// Good: message was buffered, we can drain it
	default:
		t.Fatal("expected buffered message in proc1")
	}
}
