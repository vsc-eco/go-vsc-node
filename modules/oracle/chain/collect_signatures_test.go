package chain

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/vsclog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWitness is a small helper for tests describing one elected witness:
// its DID, its election weight, and whether it will "respond" to a signature
// request. Witnesses with respond=false simulate a down/partitioned node.
type fakeWitness struct {
	did     string
	weight  uint64
	respond bool
}

// runCollectionScenario builds an in-memory signature collection harness for
// the given witnesses, feeds responses for those marked respond=true, and
// returns the collectChainSignatures result. The producer's own self-signature
// is the first witness in the list (its weight is treated as initial weight
// and its DID is treated as selfDid).
//
// totalWeight is computed as the sum of all witness weights, and threshold
// is totalWeight*2/3 — matching the production semantics in
// processChainRelay.
func runCollectionScenario(
	t *testing.T,
	witnesses []fakeWitness,
	timeout time.Duration,
) signatureCollectionResult {
	t.Helper()

	require.NotEmpty(t, witnesses, "at least one witness (self) is required")

	// Compute totalWeight + threshold using the same arithmetic as
	// processChainRelay so this test exercises the real consensus rule.
	var totalWeight uint64
	for _, w := range witnesses {
		totalWeight += w.weight
	}
	threshold := totalWeight * 2 / 3

	// First witness is "self" — its weight is the producer's initial signed
	// weight (the producer pre-signs its own data before broadcasting).
	self := witnesses[0]
	selfDid := dids.BlsDID(self.did)
	initialSignedWeight := self.weight

	// Build a weight lookup that mirrors findMemberWeight.
	weightMap := make(map[string]uint64, len(witnesses))
	for _, w := range witnesses {
		weightMap[w.did] = w.weight
	}
	weightOf := func(did dids.BlsDID) uint64 {
		return weightMap[string(did)]
	}

	// Track which DIDs have already been added so the verifier matches the
	// real BlsCircuit semantics: AddAndVerify returns added=false on duplicate.
	addedDids := make(map[string]struct{})
	var addedMu sync.Mutex
	addAndVerify := func(member dids.BlsDID, signature string) (bool, error) {
		addedMu.Lock()
		defer addedMu.Unlock()
		if _, ok := addedDids[string(member)]; ok {
			return false, nil
		}
		addedDids[string(member)] = struct{}{}
		return true, nil
	}

	// Create a channel via the real signatureChannels machinery so we
	// exercise the same code path the production producer uses.
	sc := makeSignatureChannels()
	sessionID := "TEST-" + t.Name()
	sigChan, err := sc.makeSession(sessionID)
	require.NoError(t, err)
	t.Cleanup(func() { sc.clearSession(sessionID) })

	// Feed signatures from each "responding" non-self witness in a goroutine
	// so collectChainSignatures runs in parallel with the simulated network.
	go func() {
		// Tiny stagger so all messages don't land in a single select
		// iteration — closer to real-world delivery.
		for _, w := range witnesses[1:] {
			if !w.respond {
				continue
			}
			_ = sc.receiveSignature(sessionID, signatureMessage{
				Signature: "fake-sig-for-" + w.did,
				Account:   "acct-" + w.did,
				BlsDid:    w.did,
			})
			time.Sleep(time.Millisecond)
		}
	}()

	logger := vsclog.Module("oracle-chain-test")

	return collectChainSignatures(
		context.Background(),
		logger,
		sigChan,
		timeout,
		initialSignedWeight,
		threshold,
		selfDid,
		addAndVerify,
		weightOf,
		"BTC",
	)
}

// makeWitnesses constructs a slice of witnesses with equal weight, optionally
// marking the first `down` non-self witnesses as down.
func makeWitnesses(total int, weight uint64, down int) []fakeWitness {
	out := make([]fakeWitness, total)
	for i := 0; i < total; i++ {
		out[i] = fakeWitness{
			did:     fmt.Sprintf("did:key:z6Wit%d", i),
			weight:  weight,
			respond: true,
		}
	}
	// Index 0 is "self" and always responds — mark non-self witnesses down.
	for i := 1; i <= down && i < total; i++ {
		out[i].respond = false
	}
	return out
}

// =====================================================================
// Happy path: all witnesses respond
// =====================================================================

func TestCollectChainSignatures_AllRespond_Succeeds(t *testing.T) {
	witnesses := makeWitnesses(4, 10, 0) // 4 witnesses, weight 10 each, 0 down
	// total=40, threshold = 40*2/3 = 26
	// Self pre-signs (weight 10), 3 more respond (each weight 10) -> 40
	// Loop exits after weight crosses 26 (i.e. after 2 more sigs at weight 30).

	result := runCollectionScenario(t, witnesses, 2*time.Second)

	assert.False(t, result.TimedOut, "should not time out when all witnesses respond")
	assert.False(t, result.Cancelled, "should not be cancelled")
	assert.Greater(t, result.SignedWeight, uint64(40*2/3),
		"signed weight should exceed 2/3 threshold")
}

// =====================================================================
// Happy path: enough respond (above threshold)
// =====================================================================

func TestCollectChainSignatures_OneDown_StillReachesThreshold(t *testing.T) {
	// 4 witnesses, weight 10 each, 1 down. Total=40, threshold=26.
	// Responding weight = 10 (self) + 20 (two non-self) = 30 > 26.
	witnesses := makeWitnesses(4, 10, 1)

	result := runCollectionScenario(t, witnesses, 2*time.Second)

	assert.False(t, result.TimedOut, "should still meet threshold with one down")
	assert.False(t, result.Cancelled)
	assert.Greater(t, result.SignedWeight, uint64(40*2/3),
		"signed weight should exceed 2/3 threshold even with one node down")
}

// =====================================================================
// THE BUG: not enough respond -> waits full timeout, returns under threshold
// =====================================================================

func TestCollectChainSignatures_TooManyDown_TimesOut(t *testing.T) {
	// 4 witnesses, weight 10 each, 2 down. Total=40, threshold=26.
	// Responding weight = 10 (self) + 10 (one non-self) = 20 <= 26.
	// The loop will wait the full timeout and then return TimedOut=true.
	witnesses := makeWitnesses(4, 10, 2)

	// Use a short timeout for the test so we don't wait 90s. The behavior is
	// identical — only the wall-clock duration changes.
	const testTimeout = 200 * time.Millisecond
	start := time.Now()
	result := runCollectionScenario(t, witnesses, testTimeout)
	elapsed := time.Since(start)

	assert.True(t, result.TimedOut,
		"should time out when responders carry insufficient weight")
	assert.False(t, result.Cancelled)
	assert.LessOrEqual(t, result.SignedWeight, uint64(40*2/3),
		"signed weight should remain at or below threshold")
	assert.GreaterOrEqual(t, elapsed, testTimeout,
		"should wait the full timeout before giving up — this is the productivity bug")
}

// =====================================================================
// Edge case: a single heavy-weight witness going down stalls the relay
// =====================================================================

func TestCollectChainSignatures_HeavyWitnessDown_TimesOut(t *testing.T) {
	// 4 witnesses with skewed weights — one carries 60% alone.
	// total = 100, threshold = 66
	// If the heavy witness is down: max possible = 10+10+10 = 30 (with self) -> 30 <= 66.
	witnesses := []fakeWitness{
		{did: "did:key:z6Self", weight: 10, respond: true},   // self
		{did: "did:key:z6Wit1", weight: 60, respond: false},  // heavy, down
		{did: "did:key:z6Wit2", weight: 10, respond: true},
		{did: "did:key:z6Wit3", weight: 10, respond: true},
		{did: "did:key:z6Wit4", weight: 10, respond: true},
	}

	const testTimeout = 200 * time.Millisecond
	result := runCollectionScenario(t, witnesses, testTimeout)

	assert.True(t, result.TimedOut,
		"a single heavy down witness should stall the relay even when 4 of 5 nodes respond")
	assert.LessOrEqual(t, result.SignedWeight, uint64(100*2/3))
}

// =====================================================================
// Edge case: all non-self witnesses down -> times out at self weight
// =====================================================================

func TestCollectChainSignatures_AllOthersDown_TimesOut(t *testing.T) {
	witnesses := makeWitnesses(4, 10, 3) // 3 of 3 non-self down

	const testTimeout = 200 * time.Millisecond
	result := runCollectionScenario(t, witnesses, testTimeout)

	assert.True(t, result.TimedOut)
	assert.Equal(t, uint64(10), result.SignedWeight,
		"signed weight should equal initial self weight when nobody else responds")
}

// =====================================================================
// Strict-greater-than threshold semantics: exactly 2/3 is NOT enough.
// This documents the production rule (`for signedWeight <= threshold`).
// =====================================================================

func TestCollectChainSignatures_ExactlyTwoThirds_NotEnough(t *testing.T) {
	// 3 witnesses, weight 10 each. total=30, threshold = 30*2/3 = 20.
	// Self (10) + one responder (10) = 20 == threshold => still <= threshold,
	// so the loop keeps waiting. The third witness is down.
	witnesses := []fakeWitness{
		{did: "did:key:z6Self", weight: 10, respond: true},
		{did: "did:key:z6Wit1", weight: 10, respond: true},
		{did: "did:key:z6Wit2", weight: 10, respond: false},
	}

	const testTimeout = 200 * time.Millisecond
	result := runCollectionScenario(t, witnesses, testTimeout)

	assert.True(t, result.TimedOut,
		"signedWeight == threshold is NOT enough; production uses strict greater-than")
	assert.Equal(t, uint64(20), result.SignedWeight)
}

// =====================================================================
// Self-signature in the channel is ignored (no double counting)
// =====================================================================

func TestCollectChainSignatures_SelfSignatureSkipped(t *testing.T) {
	logger := vsclog.Module("oracle-chain-test")

	sc := makeSignatureChannels()
	sigChan, err := sc.makeSession("self-skip")
	require.NoError(t, err)
	t.Cleanup(func() { sc.clearSession("self-skip") })

	selfDid := dids.BlsDID("did:key:z6Self")

	// Send self's signature into the channel — collector must ignore it.
	go func() {
		_ = sc.receiveSignature("self-skip", signatureMessage{
			Signature: "self-sig",
			Account:   "self",
			BlsDid:    string(selfDid),
		})
	}()

	addAndVerify := func(member dids.BlsDID, signature string) (bool, error) {
		t.Errorf("addAndVerify should not be called for self signature, got %s", member)
		return true, nil
	}
	weightOf := func(did dids.BlsDID) uint64 { return 10 }

	result := collectChainSignatures(
		context.Background(),
		logger,
		sigChan,
		100*time.Millisecond,
		10, // self weight
		20, // threshold
		selfDid,
		addAndVerify,
		weightOf,
		"BTC",
	)

	assert.True(t, result.TimedOut,
		"with only self-signature in the channel, threshold is never met")
	assert.Equal(t, uint64(10), result.SignedWeight)
}

// =====================================================================
// Empty / malformed messages are ignored
// =====================================================================

func TestCollectChainSignatures_EmptyMessagesIgnored(t *testing.T) {
	logger := vsclog.Module("oracle-chain-test")
	sc := makeSignatureChannels()
	sigChan, err := sc.makeSession("empty-msg")
	require.NoError(t, err)
	t.Cleanup(func() { sc.clearSession("empty-msg") })

	go func() {
		// Empty BlsDid
		_ = sc.receiveSignature("empty-msg", signatureMessage{
			Signature: "abc",
			Account:   "noone",
			BlsDid:    "",
		})
		// Empty signature
		_ = sc.receiveSignature("empty-msg", signatureMessage{
			Signature: "",
			Account:   "noone",
			BlsDid:    "did:key:z6Wit1",
		})
	}()

	verifyCalls := 0
	addAndVerify := func(member dids.BlsDID, signature string) (bool, error) {
		verifyCalls++
		return true, nil
	}
	weightOf := func(did dids.BlsDID) uint64 { return 10 }

	result := collectChainSignatures(
		context.Background(),
		logger,
		sigChan,
		100*time.Millisecond,
		10, // self
		20, // threshold
		dids.BlsDID("did:key:z6Self"),
		addAndVerify,
		weightOf,
		"BTC",
	)

	assert.True(t, result.TimedOut)
	assert.Equal(t, 0, verifyCalls,
		"empty messages should never reach the verifier")
}

// =====================================================================
// Verifier rejects (added=false) — should not increment weight
// =====================================================================

func TestCollectChainSignatures_VerifierRejects(t *testing.T) {
	logger := vsclog.Module("oracle-chain-test")
	sc := makeSignatureChannels()
	sigChan, err := sc.makeSession("reject")
	require.NoError(t, err)
	t.Cleanup(func() { sc.clearSession("reject") })

	go func() {
		_ = sc.receiveSignature("reject", signatureMessage{
			Signature: "bogus",
			Account:   "wit1",
			BlsDid:    "did:key:z6Wit1",
		})
	}()

	addAndVerify := func(member dids.BlsDID, signature string) (bool, error) {
		return false, nil // mirror BlsCircuit duplicate/invalid behaviour
	}
	weightOf := func(did dids.BlsDID) uint64 { return 100 }

	result := collectChainSignatures(
		context.Background(),
		logger,
		sigChan,
		100*time.Millisecond,
		10, // self
		20, // threshold
		dids.BlsDID("did:key:z6Self"),
		addAndVerify,
		weightOf,
		"BTC",
	)

	assert.True(t, result.TimedOut,
		"rejected signatures should not count toward threshold")
	assert.Equal(t, uint64(10), result.SignedWeight)
}

// =====================================================================
// Verifier errors — same: should not increment weight
// =====================================================================

func TestCollectChainSignatures_VerifierErrors(t *testing.T) {
	logger := vsclog.Module("oracle-chain-test")
	sc := makeSignatureChannels()
	sigChan, err := sc.makeSession("error")
	require.NoError(t, err)
	t.Cleanup(func() { sc.clearSession("error") })

	go func() {
		_ = sc.receiveSignature("error", signatureMessage{
			Signature: "bad",
			Account:   "wit1",
			BlsDid:    "did:key:z6Wit1",
		})
	}()

	addAndVerify := func(member dids.BlsDID, signature string) (bool, error) {
		return false, fmt.Errorf("verification failed")
	}
	weightOf := func(did dids.BlsDID) uint64 { return 100 }

	result := collectChainSignatures(
		context.Background(),
		logger,
		sigChan,
		100*time.Millisecond,
		10,
		20,
		dids.BlsDID("did:key:z6Self"),
		addAndVerify,
		weightOf,
		"BTC",
	)

	assert.True(t, result.TimedOut)
	assert.Equal(t, uint64(10), result.SignedWeight)
}

// =====================================================================
// Context cancellation mid-collection
// =====================================================================

func TestCollectChainSignatures_ContextCancelled(t *testing.T) {
	logger := vsclog.Module("oracle-chain-test")
	sc := makeSignatureChannels()
	sigChan, err := sc.makeSession("ctx-cancel")
	require.NoError(t, err)
	t.Cleanup(func() { sc.clearSession("ctx-cancel") })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	addAndVerify := func(member dids.BlsDID, signature string) (bool, error) {
		return true, nil
	}
	weightOf := func(did dids.BlsDID) uint64 { return 10 }

	result := collectChainSignatures(
		ctx,
		logger,
		sigChan,
		5*time.Second, // long enough that cancellation is the cause
		10,
		1000,
		dids.BlsDID("did:key:z6Self"),
		addAndVerify,
		weightOf,
		"BTC",
	)

	assert.True(t, result.Cancelled)
	assert.False(t, result.TimedOut)
}

// =====================================================================
// Channel closed externally is treated as cancellation
// =====================================================================

func TestCollectChainSignatures_ChannelClosed(t *testing.T) {
	logger := vsclog.Module("oracle-chain-test")

	closableChan := make(chan signatureMessage)
	close(closableChan)

	addAndVerify := func(member dids.BlsDID, signature string) (bool, error) {
		return true, nil
	}
	weightOf := func(did dids.BlsDID) uint64 { return 10 }

	result := collectChainSignatures(
		context.Background(),
		logger,
		closableChan,
		5*time.Second,
		10,
		100,
		dids.BlsDID("did:key:z6Self"),
		addAndVerify,
		weightOf,
		"BTC",
	)

	assert.True(t, result.Cancelled,
		"closed channel should be treated as cancellation, not timeout")
}

// =====================================================================
// Producer's initial weight already meets threshold — no waiting
// =====================================================================

func TestCollectChainSignatures_InitialWeightAlreadyAboveThreshold(t *testing.T) {
	logger := vsclog.Module("oracle-chain-test")
	sc := makeSignatureChannels()
	sigChan, err := sc.makeSession("instant")
	require.NoError(t, err)
	t.Cleanup(func() { sc.clearSession("instant") })

	addAndVerify := func(member dids.BlsDID, signature string) (bool, error) {
		t.Error("should not receive any signatures")
		return true, nil
	}
	weightOf := func(did dids.BlsDID) uint64 { return 0 }

	start := time.Now()
	result := collectChainSignatures(
		context.Background(),
		logger,
		sigChan,
		5*time.Second,
		100, // self already above threshold
		50,  // threshold
		dids.BlsDID("did:key:z6Self"),
		addAndVerify,
		weightOf,
		"BTC",
	)
	elapsed := time.Since(start)

	assert.False(t, result.TimedOut)
	assert.False(t, result.Cancelled)
	assert.Equal(t, uint64(100), result.SignedWeight)
	assert.Less(t, elapsed, 100*time.Millisecond,
		"should return instantly when initial weight already exceeds threshold")
}
