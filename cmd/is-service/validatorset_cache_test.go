package main

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round-6 audit R6-TEST-01: cache tests now exercise the REAL
// validatorSetCache.Get + GC methods through the validatorSetFetcher
// interface (extracted in round-6). The pre-R6 testCache shadow has
// been deleted — a future regression in production Get() now fails
// CI instead of silently passing.

type fakeFetcher struct {
	fn    func(ctx context.Context, contractID string, epoch uint64) (map[string]string, error)
	calls atomic.Int32
}

func (f *fakeFetcher) FetchValidatorSet(ctx context.Context, contractID string, epoch uint64) (map[string]string, error) {
	f.calls.Add(1)
	return f.fn(ctx, contractID, epoch)
}

func TestValidatorSetCache_HitWithinTTL(t *testing.T) {
	f := &fakeFetcher{fn: func(_ context.Context, _ string, _ uint64) (map[string]string, error) {
		return map[string]string{"did:key:a": "pk-a"}, nil
	}}
	c := newValidatorSetCache(f, "", time.Minute)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		v := c.Get(ctx, 7)
		assert.Equal(t, "pk-a", v["did:key:a"])
	}
	assert.Equal(t, int32(1), f.calls.Load(), "TTL-window calls must coalesce")
}

func TestValidatorSetCache_SingleflightCoalesces(t *testing.T) {
	var inFlight atomic.Int32
	var maxConcurrent atomic.Int32
	release := make(chan struct{})
	f := &fakeFetcher{fn: func(_ context.Context, _ string, _ uint64) (map[string]string, error) {
		cur := inFlight.Add(1)
		for {
			old := maxConcurrent.Load()
			if cur <= old || maxConcurrent.CompareAndSwap(old, cur) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		return map[string]string{"did:key:a": "pk-a"}, nil
	}}
	c := newValidatorSetCache(f, "", time.Minute)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Get(ctx, 7)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), maxConcurrent.Load(),
		"singleflight must allow at most one in-flight fetch per epoch")
}

func TestValidatorSetCache_ErrorPreservesPreviousValue(t *testing.T) {
	// Round-5 R5-006 + round-6 R6-TEST-01: a transient fetch error
	// MUST NOT evict the previous good entry.
	var phase atomic.Int32
	f := &fakeFetcher{fn: func(_ context.Context, _ string, _ uint64) (map[string]string, error) {
		if phase.Load() == 0 {
			return map[string]string{"did:key:a": "pk-a"}, nil
		}
		return nil, errors.New("graphql flap")
	}}
	c := newValidatorSetCache(f, "", 60*time.Second)
	ctx := context.Background()
	v := c.Get(ctx, 7)
	require.NotNil(t, v)
	assert.Equal(t, "pk-a", v["did:key:a"])

	// Force the next fetch to fail by expiring the entry.
	c.mu.Lock()
	c.entries[7].expiresAt = time.Now().Add(-time.Second)
	c.mu.Unlock()
	phase.Store(1)

	v2 := c.Get(ctx, 7)
	assert.NotNil(t, v2, "fetch error must NOT poison cache")
	assert.Equal(t, "pk-a", v2["did:key:a"],
		"prior good entry must remain readable after transient fetch error")
}

func TestValidatorSetCache_DifferentEpochsDontShareLoadCh(t *testing.T) {
	f := &fakeFetcher{fn: func(_ context.Context, _ string, epoch uint64) (map[string]string, error) {
		return map[string]string{"did:" + strconv.FormatUint(epoch, 10): "pk"}, nil
	}}
	c := newValidatorSetCache(f, "", time.Minute)
	ctx := context.Background()
	v1 := c.Get(ctx, 1)
	v2 := c.Get(ctx, 2)
	v3 := c.Get(ctx, 3)
	assert.NotNil(t, v1["did:1"])
	assert.NotNil(t, v2["did:2"])
	assert.NotNil(t, v3["did:3"])
	assert.Equal(t, int32(3), f.calls.Load(),
		"each epoch must trigger its own fetch")
}

func TestValidatorSetCache_CtxCancelExitsWaiter(t *testing.T) {
	release := make(chan struct{})
	f := &fakeFetcher{fn: func(_ context.Context, _ string, _ uint64) (map[string]string, error) {
		<-release
		return map[string]string{"did:key:a": "pk-a"}, nil
	}}
	c := newValidatorSetCache(f, "", time.Minute)
	go func() { _ = c.Get(context.Background(), 7) }()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan map[string]string, 1)
	go func() {
		done <- c.Get(ctx, 7)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case v := <-done:
		assert.Nil(t, v, "cancelled waiter must return nil")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Get did not return after ctx cancel")
	}
	close(release)
}

// Round-6 audit R6-TEST-04: GC tests for the R5-OP-05 method.
func TestValidatorSetCache_GC_DropsExpiredEntries(t *testing.T) {
	f := &fakeFetcher{fn: func(_ context.Context, _ string, _ uint64) (map[string]string, error) {
		return map[string]string{"did:key:a": "pk-a"}, nil
	}}
	c := newValidatorSetCache(f, "", time.Minute)
	ctx := context.Background()
	_ = c.Get(ctx, 1)
	_ = c.Get(ctx, 2)
	// Push entry 1 well past expiresAt.
	c.mu.Lock()
	c.entries[1].expiresAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	removed := c.GC(5 * time.Minute)
	assert.Equal(t, 1, removed, "old entry must be dropped")
	c.mu.Lock()
	_, has1 := c.entries[1]
	_, has2 := c.entries[2]
	c.mu.Unlock()
	assert.False(t, has1)
	assert.True(t, has2, "fresh entry must be preserved")
}

func TestValidatorSetCache_GC_PreservesLoadingEntry(t *testing.T) {
	release := make(chan struct{})
	f := &fakeFetcher{fn: func(_ context.Context, _ string, _ uint64) (map[string]string, error) {
		<-release
		return map[string]string{"did:key:a": "pk-a"}, nil
	}}
	c := newValidatorSetCache(f, "", time.Minute)
	go func() { _ = c.Get(context.Background(), 7) }()
	time.Sleep(20 * time.Millisecond)
	// GC must not drop the in-flight entry even if its expiresAt
	// is zero (default). Otherwise the loader's eventual write
	// would create a phantom entry.
	removed := c.GC(0)
	assert.Equal(t, 0, removed,
		"GC must skip entries with loading != nil")
	close(release)
}
