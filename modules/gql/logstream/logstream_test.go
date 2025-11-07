package logstream

import (
	"sync"
	"testing"
	"time"
)

// TestSubscribeAndUnsubscribe verifies that subscribing and unsubscribing
// works correctly and that published logs are received while subscribed.
func TestSubscribeAndUnsubscribe(t *testing.T) {
	ls := NewLogStream()
	sub := ls.Subscribe(LogFilterInternal{})
	if sub == nil {
		t.Fatal("expected non-nil subscriber")
	}

	// Publish a log and verify subscriber receives it.
	testLog := ContractLog{BlockHeight: 10, ContractAddress: "addr1", Log: "event"}
	ls.Publish(testLog)

	select {
	case got := <-sub.Ch:
		if got.Log != "event" {
			t.Errorf("expected log 'event', got %s", got.Log)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for log")
	}

	// Unsubscribe should close the channel.
	ls.Unsubscribe(sub)
	_, ok := <-sub.Ch
	if ok {
		t.Error("expected channel closed after unsubscribe")
	}
}

// TestFilterByBlockAndAddress ensures that FromBlock and ContractAddresses filters
// are respected — only matching logs should be delivered.
func TestFilterByBlockAndAddress(t *testing.T) {
	minBlock := uint64(5)
	filter := LogFilterInternal{
		FromBlock:         &minBlock,
		ContractAddresses: map[string]struct{}{"addr1": {}},
	}

	ls := NewLogStream()
	sub := ls.Subscribe(filter)

	// Lower block should be ignored.
	ls.Publish(ContractLog{BlockHeight: 4, ContractAddress: "addr1", Log: "ignore"})
	// Different address should be ignored.
	ls.Publish(ContractLog{BlockHeight: 6, ContractAddress: "addr2", Log: "ignore"})
	// Matching log should be delivered.
	ls.Publish(ContractLog{BlockHeight: 6, ContractAddress: "addr1", Log: "match"})

	select {
	case l := <-sub.Ch:
		if l.Log != "match" {
			t.Errorf("expected 'match', got %s", l.Log)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for filtered log")
	}
}

// TestConcurrentPublish stresses the Publish function under concurrent load.
func TestConcurrentPublish(t *testing.T) {
	ls := NewLogStream()
	sub := ls.Subscribe(LogFilterInternal{})
	wg := sync.WaitGroup{}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ls.Publish(ContractLog{BlockHeight: uint64(j), Log: "ok"})
			}
		}()
	}
	wg.Wait()
	ls.Unsubscribe(sub)
}

// TestChannelOverflow ensures that Publish never blocks even when a channel is full.
func TestChannelOverflow(t *testing.T) {
	ls := NewLogStream()
	sub := ls.Subscribe(LogFilterInternal{})
	defer ls.Unsubscribe(sub)

	// Fill up the subscriber channel
	for i := 0; i < cap(sub.Ch); i++ {
		ls.Publish(ContractLog{BlockHeight: uint64(i), Log: "fill"})
	}

	// This one should be dropped, not block or panic
	done := make(chan struct{})
	go func() {
		ls.Publish(ContractLog{BlockHeight: 999, Log: "overflow"})
		close(done)
	}()

	select {
	case <-done:
		// success — Publish returned quickly
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Publish blocked when channel full")
	}
}

// TestReplayPublishesLogs checks that Replay calls its source and publishes logs in order.
func TestReplayPublishesLogs(t *testing.T) {
	ls := NewLogStream()
	sub := ls.Subscribe(LogFilterInternal{})
	defer ls.Unsubscribe(sub)

	called := false
	source := func(from, to uint64, fn func(ContractLog) error) error {
		called = true
		logs := []ContractLog{
			{BlockHeight: 1, Log: "L1"},
			{BlockHeight: 2, Log: "L2"},
		}
		for _, l := range logs {
			if err := fn(l); err != nil {
				return err
			}
		}
		return nil
	}

	err := ls.Replay(1, 2, source)
	if err != nil {
		t.Fatalf("Replay returned error: %v", err)
	}
	if !called {
		t.Error("Replay did not call logsSource")
	}

	var received []string
	for i := 0; i < 2; i++ {
		select {
		case l := <-sub.Ch:
			received = append(received, l.Log)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for replayed logs")
		}
	}

	if received[0] != "L1" || received[1] != "L2" {
		t.Errorf("unexpected replay order: %v", received)
	}
}

// TestCurrentHeight verifies that the latest block height increases correctly.
func TestCurrentHeight(t *testing.T) {
	ls := NewLogStream()
	ls.Publish(ContractLog{BlockHeight: 10})
	ls.Publish(ContractLog{BlockHeight: 15})
	ls.Publish(ContractLog{BlockHeight: 5}) // should not decrease

	if got := ls.CurrentHeight(); got != 15 {
		t.Errorf("expected height 15, got %d", got)
	}
}

// TestReplayEmptySource ensures Replay handles an empty source without error.
func TestReplayEmptySource(t *testing.T) {
	ls := NewLogStream()

	// Updated mock source for streaming form
	source := func(from, to uint64, fn func(ContractLog) error) error {
		// simulate no logs at all
		return nil
	}

	if err := ls.Replay(0, 10, source); err != nil {
		t.Fatalf("expected no error on empty source, got %v", err)
	}

	if ls.CurrentHeight() != 0 {
		t.Errorf("expected no update to height, got %d", ls.CurrentHeight())
	}
}

// TestTimestampPropagation ensures timestamps are forwarded correctly.
func TestTimestampPropagation(t *testing.T) {
	ls := NewLogStream()
	sub := ls.Subscribe(LogFilterInternal{})
	defer ls.Unsubscribe(sub)

	ls.Publish(ContractLog{BlockHeight: 42, Log: "log", Timestamp: "2025-11-07T12:00:00Z"})

	select {
	case l := <-sub.Ch:
		if l.Timestamp != "2025-11-07T12:00:00Z" {
			t.Errorf("expected timestamp propagation, got %s", l.Timestamp)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for log with timestamp")
	}
}
