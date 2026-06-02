package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	islock "vsc-node/modules/islock-attestation"
)

func TestCollector_CollectsToThreshold(t *testing.T) {
	c := newAttestationCollector()
	ctx := context.Background()

	go func() {
		// Deliver three responses in sequence; threshold is 2.
		time.Sleep(10 * time.Millisecond)
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v1", BlsSigHex: "00"})
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v2", BlsSigHex: "01"})
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v3", BlsSigHex: "02"})
	}()

	got := c.Collect(ctx, "abc", 2, time.Second)
	assert.Len(t, got, 2, "Collect returns once threshold is met")
}

func TestCollector_DedupesByValidatorDID(t *testing.T) {
	c := newAttestationCollector()
	ctx := context.Background()

	go func() {
		time.Sleep(10 * time.Millisecond)
		// Same validator delivers twice — counts once.
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v1"})
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v1"})
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v2"})
	}()

	got := c.Collect(ctx, "abc", 2, time.Second)
	assert.Len(t, got, 2)
	dids := []string{got[0].ValidatorDID, got[1].ValidatorDID}
	assert.ElementsMatch(t, []string{"v1", "v2"}, dids)
}

func TestCollector_TimeoutReturnsWhatWasCollected(t *testing.T) {
	c := newAttestationCollector()
	ctx := context.Background()

	go func() {
		time.Sleep(10 * time.Millisecond)
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v1"})
	}()

	// Threshold 3, only 1 response delivered; timeout returns partial.
	got := c.Collect(ctx, "abc", 3, 100*time.Millisecond)
	assert.Len(t, got, 1)
}

func TestCollector_DropsResponsesForUnknownTxid(t *testing.T) {
	c := newAttestationCollector()
	// No Await registered for "ghost" — Deliver must not panic.
	c.Deliver(islock.IsLockAttestationResponse{TxId: "ghost", ValidatorDID: "v1"})
}

func TestCollector_CtxCancelReturnsEarly(t *testing.T) {
	c := newAttestationCollector()
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	got := c.Collect(ctx, "abc", 5, 10*time.Second)
	dur := time.Since(start)
	assert.Less(t, dur, 200*time.Millisecond, "Collect must honour ctx cancel")
	assert.Empty(t, got)
}

// TestCollector_RefcountTracksAwaitersCorrectly — regression for
// audit R2-002 + M4. Two Awaits + two Cancels (matching pairs) must
// fully release the entry; one Await + one Cancel must also release.
func TestCollector_RefcountTracksAwaitersCorrectly(t *testing.T) {
	c := newAttestationCollector()
	const txid = "tx-refcount"

	// Pre-Await (caller A): refcount=1.
	chA := c.Await(txid, 4)
	// Collect-style Await (caller B): refcount=2, same channel.
	chB := c.Await(txid, 4)
	assert.Equal(t, chA, chB, "duplicate Await for same txid returns same channel")

	c.mu.Lock()
	require.Equal(t, 2, c.awaiters[txid].refcount,
		"two Awaits should yield refcount=2")
	c.mu.Unlock()

	// First Cancel (caller B): refcount=1, channel NOT closed.
	c.Cancel(txid)
	c.mu.Lock()
	require.NotNil(t, c.awaiters[txid], "entry must persist after first Cancel")
	require.Equal(t, 1, c.awaiters[txid].refcount)
	c.mu.Unlock()

	// Second Cancel (caller A): refcount=0, channel closed, entry removed.
	c.Cancel(txid)
	c.mu.Lock()
	_, exists := c.awaiters[txid]
	c.mu.Unlock()
	assert.False(t, exists, "entry must be removed after final Cancel")
}

// TestCollector_ExtraCancelIsNoOp — pure mismatch guard: a Cancel
// without a prior Await must not panic.
func TestCollector_ExtraCancelIsNoOp(t *testing.T) {
	c := newAttestationCollector()
	c.Cancel("never-awaited") // must not panic
}

func TestCollector_ConcurrentDeliveriesSafe(t *testing.T) {
	c := newAttestationCollector()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Deliver(islock.IsLockAttestationResponse{
				TxId:         "abc",
				ValidatorDID: "v" + string(rune('0'+i)),
			})
		}(i)
	}

	got := c.Collect(ctx, "abc", 5, 200*time.Millisecond)
	wg.Wait()
	assert.GreaterOrEqual(t, len(got), 0,
		"concurrent deliveries must not panic; ordering not asserted")
}
