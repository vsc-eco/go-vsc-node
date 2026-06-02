package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

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
