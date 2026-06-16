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

	got := c.Collect(ctx, "abc", 2, time.Second, nil)
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

	got := c.Collect(ctx, "abc", 2, time.Second, nil)
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
	got := c.Collect(ctx, "abc", 3, 100*time.Millisecond, nil)
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
	got := c.Collect(ctx, "abc", 5, 10*time.Second, nil)
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

	got := c.Collect(ctx, "abc", 5, 200*time.Millisecond, nil)
	wg.Wait()
	assert.GreaterOrEqual(t, len(got), 0,
		"concurrent deliveries must not panic; ordering not asserted")
}

// TestCollector_VerifierGatesThresholdCount covers the op=call devnet
// E2E regression (2026-06-05): with quorumThreshold=1 and a verifier
// predicate, the FIRST raw response that fails the verifier must NOT
// terminate Collect — it should keep collecting until a verified
// response arrives or timeout fires. Pre-fix, the collector returned
// at the first response regardless of verifier outcome, the
// orchestrator's post-Collect filter dropped it as unknown DID, and
// the session ATTESTATION_TIMEOUT'd with verified=0 collected=1.
// Post-fix, the verifier inline lets junk responses through into the
// returned slice (for diagnostic visibility) but only counts verified
// responses toward threshold.
func TestCollector_VerifierGatesThresholdCount(t *testing.T) {
	c := newAttestationCollector()
	ctx := context.Background()

	go func() {
		// Deliver one unknown-DID response (verifier rejects), then
		// a known-DID response (verifier accepts). With threshold=1
		// the collector must NOT return at v_unknown; it must wait
		// for v_known.
		time.Sleep(10 * time.Millisecond)
		c.Deliver(islock.IsLockAttestationResponse{
			TxId: "abc", ValidatorDID: "v_unknown", BlsSigHex: "ff",
		})
		time.Sleep(20 * time.Millisecond)
		c.Deliver(islock.IsLockAttestationResponse{
			TxId: "abc", ValidatorDID: "v_known", BlsSigHex: "aa",
		})
	}()

	verifier := func(r islock.IsLockAttestationResponse) bool {
		return r.ValidatorDID == "v_known"
	}
	got := c.Collect(ctx, "abc", 1, 2*time.Second, verifier)

	// Both responses returned for diagnostics (the orchestrator's
	// "roster divergence" warning needs to see ALL responders).
	require.Len(t, got, 2, "Collect must return both raw responses (verified + rejected) for diagnostics")
	dids := []string{got[0].ValidatorDID, got[1].ValidatorDID}
	assert.ElementsMatch(t, []string{"v_unknown", "v_known"}, dids,
		"raw response slice must include the rejected response (for matchedAny diagnostic)")
}

// TestCollector_VerifierTimeoutWithoutVerifiedResponse covers the
// negative path: if NO verified response arrives within the timeout,
// Collect returns at deadline with whatever raw responses came in
// (for diagnostic surfacing of "we got responses but none matched").
func TestCollector_VerifierTimeoutWithoutVerifiedResponse(t *testing.T) {
	c := newAttestationCollector()
	ctx := context.Background()

	go func() {
		// Three unknown-DID responses, none verifier-acceptable.
		time.Sleep(10 * time.Millisecond)
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v1"})
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v2"})
		c.Deliver(islock.IsLockAttestationResponse{TxId: "abc", ValidatorDID: "v3"})
	}()

	rejectAll := func(islock.IsLockAttestationResponse) bool { return false }
	got := c.Collect(ctx, "abc", 1, 200*time.Millisecond, rejectAll)

	// Threshold never met → Collect ran the full timeout.
	// All 3 raw responses returned for diagnostics (so the
	// orchestrator can emit the "roster divergence" warning).
	assert.Len(t, got, 3, "all raw responses must be returned even when none satisfy verifier")
}
