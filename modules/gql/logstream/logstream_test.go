package logstream

import (
	"sync"
	"testing"
	"time"
)

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
