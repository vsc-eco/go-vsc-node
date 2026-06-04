package islock_attestation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bls "github.com/protolambda/bls12-381-util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingSigner wraps a real BLS signer + records how many times
// Sign() was called. Used to prove the cache short-circuits Sign() on
// rebroadcast.
//
// signCalls is an atomic.Int64 (audit R20-SEC-singleflight-test-
// signcalls-non-atomic-race-on-failure): the prior int field with
// `c.signCalls++` had a non-atomic increment that the race detector
// flagged when the singleflight path is broken. The atomic counter
// is itself race-safe and lets the race detector's signal fire on
// any OTHER unsafe sharing.
//
// signBarrier (optional): if non-nil, the FIRST Sign call blocks on
// <-signBarrier until the test releases it. Used by the singleflight
// test to force overlap between the leader's Sign and follower
// register-on-the-gate window (audit R20-CORR-singleflight-test-
// does-not-force-race: without a barrier the test could pass even
// if singleflight is a no-op, because the Go scheduler may serialise
// goroutines on a single P).
type countingSigner struct {
	inner       *fakeSigner
	signCalls   atomic.Int64
	signBarrier chan struct{}
	// signPanic, when non-nil, is invoked on EVERY Sign() call (no
	// first-call guard) with the request that triggered it; if it
	// returns a non-nil value, Sign panics with that value. Used by
	// the panic-safety test (audit R20-CORR-singleflight-panic-leaks-
	// followers-forever). Tests that want one-shot semantics MUST
	// nil-out the field after observing the panic (see the panic test
	// for the pattern). Audit R21-CONS-signpanic-godoc-says-first-but-
	// code-checks-every-call: the prior godoc claimed FIRST-call only,
	// but the code at Sign() checks the field unconditionally — the
	// honest contract is "always fires while non-nil".
	signPanic func(IsLockAttestationRequest) any
}

func newCountingSigner(t *testing.T, did string) *countingSigner {
	t.Helper()
	var skBytes [32]byte
	_, err := rand.Read(skBytes[:])
	require.NoError(t, err)
	var sk bls.SecretKey
	require.NoError(t, sk.Deserialize(&skBytes))
	pk, err := bls.SkToPk(&sk)
	require.NoError(t, err)
	pkBytes := pk.Serialize()
	return &countingSigner{
		inner: &fakeSigner{did: did, sk: &sk, pkHex: hex.EncodeToString(pkBytes[:])},
	}
}

func (c *countingSigner) ValidatorDID() string { return c.inner.did }
func (c *countingSigner) PubkeyHex() string    { return c.inner.pkHex }
func (c *countingSigner) Sign(req IsLockAttestationRequest) (string, error) {
	// Increment FIRST so the test can assert ordering: when the
	// barrier blocks the leader, the test sees signCalls==1 BEFORE
	// releasing the barrier and observing the followers register.
	c.signCalls.Add(1)
	if c.signBarrier != nil {
		<-c.signBarrier
		// Once the test does close(c.signBarrier), every subsequent
		// <-c.signBarrier returns immediately with the zero value
		// (closed-channel broadcast semantics). No nil-out needed —
		// in the singleflight test only the leader's Sign call hits
		// this branch (followers replay from cache), but a future test
		// that re-enters Sign multiple times is also safe because
		// closed-channel reads are non-blocking + race-free. Audit
		// R21-CONS-signbarrier-comment-claims-nil-out-but-test-uses-
		// close: the prior comment described a nil-out mechanism the
		// tests do not implement.
	}
	if c.signPanic != nil {
		if v := c.signPanic(req); v != nil {
			panic(v)
		}
	}
	return c.inner.Sign(req)
}

// TestHandleRequest_DedupesAcrossRebroadcasts covers audit
// R17-SEC-dash-rebroadcast-validator-no-per-txid-sign-dedupe.
//
// Pre-fix: the IS service's R16 6s rebroadcast ticker republished the
// SAME attestation request up to 3 times per session. The validator's
// handleRequest path called s.signer.Sign(req) on EVERY rebroadcast —
// 3× the BLS-sign CPU cost per session, and up to 100×/min per
// malicious peer at the rate-limit ceiling.
//
// Post-fix: the validator caches the signed response keyed by
// (TxId, RawTxHashHex, InstructionHashHex, Epoch). A rebroadcast of an
// identical request short-circuits to the cached response without
// re-signing.
func TestHandleRequest_DedupesAcrossRebroadcasts(t *testing.T) {
	signer := newCountingSigner(t, "did:key:validator-1")
	const hex32 = "0011223344556677889900112233445566778899001122334455667788990011"
	svc := NewService("vsc-testnet", signer, fakeMemoryFor(t, hex32), nil)

	req := IsLockAttestationRequest{
		TxId:               hex32,
		RawTxHashHex:       hex32,
		InstructionHashHex: hex32,
		Epoch:              1,
		ChainId:            "vsc-testnet",
	}

	// Capture every sent response.
	var sent []*p2pMessage
	send := func(m p2pMessage) error {
		// Copy so we capture each response distinctly.
		mc := m
		sent = append(sent, &mc)
		return nil
	}

	// 3 rebroadcasts of the SAME request — simulates R16's worst-case
	// rebroadcast pattern within the 15s collect window.
	svc.handleRequest(context.Background(), req, send)
	svc.handleRequest(context.Background(), req, send)
	svc.handleRequest(context.Background(), req, send)

	require.Len(t, sent, 3, "all 3 rebroadcasts must produce a response (cached or fresh)")
	assert.Equal(t, int64(1), signer.signCalls.Load(),
		"validator must call Sign() exactly ONCE across the 3 rebroadcasts — "+
			"audit R17 says re-signing on every rebroadcast is the CPU-burn vector")

	// Every response must be byte-identical: same signature, same
	// validator DID, same pubkey, same epoch.
	for i, m := range sent {
		require.NotNil(t, m.Response, "response %d nil", i)
		assert.Equal(t, sent[0].Response.BlsSigHex, m.Response.BlsSigHex,
			"cached response sig must equal fresh response sig (idempotent replay)")
	}
}

// TestHandleRequest_NoDedupeAcrossDistinctRequests asserts the cache
// DOESN'T over-dedupe: a request with a different RawTxHashHex (e.g.
// a real different attestation) must hit Sign() fresh.
func TestHandleRequest_NoDedupeAcrossDistinctRequests(t *testing.T) {
	signer := newCountingSigner(t, "did:key:validator-1")
	const hexA = "0011223344556677889900112233445566778899001122334455667788990011"
	const hexB = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	// Memory has BOTH txids observed (with matching reverse-of-display
	// internal hashes for SEC-5).
	memA := fakeMemoryFor(t, hexA)
	svc := NewService("vsc-testnet", signer, &dualMemory{a: memA.rawTxHash, b: ReverseBytesCopy([]byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}), txA: hexA, txB: hexB}, nil)

	send := func(p2pMessage) error { return nil }

	reqA := IsLockAttestationRequest{
		TxId: hexA, RawTxHashHex: hexA, InstructionHashHex: hexA, Epoch: 1, ChainId: "vsc-testnet",
	}
	reqB := IsLockAttestationRequest{
		TxId: hexB, RawTxHashHex: hexB, InstructionHashHex: hexA, Epoch: 1, ChainId: "vsc-testnet",
	}

	svc.handleRequest(context.Background(), reqA, send)
	svc.handleRequest(context.Background(), reqB, send)

	assert.Equal(t, int64(2), signer.signCalls.Load(),
		"distinct requests must each trigger a fresh Sign(); cache must "+
			"only collapse rebroadcasts of the SAME (txid, rawTxHash, "+
			"instructionHash, epoch) tuple")
}

// TestSignCache_ExpiresAfterTTL pins the 60s TTL behaviour directly.
func TestSignCache_ExpiresAfterTTL(t *testing.T) {
	cache := newSignRequestCache()
	t0 := time.Unix(1_000_000, 0)
	cache.now = func() time.Time { return t0 }

	const key = "k"
	resp := &IsLockAttestationResponse{TxId: "tx", Epoch: 1}
	cache.put(key, resp)

	got, ok := cache.get(key)
	require.True(t, ok)
	assert.Equal(t, resp, got)

	// Advance past the TTL.
	cache.now = func() time.Time { return t0.Add(signCacheTTL + time.Second) }
	_, ok = cache.get(key)
	assert.False(t, ok, "entry must expire after signCacheTTL")
}

// TestSignCache_CapacityEvictsOldest verifies FIFO eviction at the
// 1000-entry cap.
func TestSignCache_CapacityEvictsOldest(t *testing.T) {
	cache := newSignRequestCache()

	for i := 0; i < signCacheCapacity+10; i++ {
		key := "k" + hex.EncodeToString([]byte{byte(i), byte(i >> 8)})
		cache.put(key, &IsLockAttestationResponse{TxId: key})
	}
	// First-inserted keys are evicted.
	firstKey := "k" + hex.EncodeToString([]byte{0, 0})
	_, ok := cache.get(firstKey)
	assert.False(t, ok, "FIFO eviction must drop the oldest entry once capacity is exceeded")

	// Most-recent key still cached.
	lastIdx := signCacheCapacity + 9
	lastKey := "k" + hex.EncodeToString([]byte{byte(lastIdx), byte(lastIdx >> 8)})
	_, ok = cache.get(lastKey)
	assert.True(t, ok, "most-recent entry must remain")
}

// TestSignCache_ExpireReputDoesNotEvictFreshEntry covers audit
// R18-SEC-sign-cache-fifo-duplicate-on-expire-reput +
// R18-CORR-sign-cache-fifo-dup-on-expired-reput.
//
// Pre-R18: get() on an expired entry called delete(entries, key) but
// LEFT the fifo slot in place. A subsequent put(key) appended a SECOND
// fifo entry for the same key. Eventually eviction popped the stale
// fifo slot and called delete(entries, key) — destroying the FRESH
// entry that should have been cached for its full 60s TTL.
//
// Post-R18: get-expire ALSO splices the matching fifo entry, so a
// re-put creates exactly one fifo slot per live key.
func TestSignCache_ExpireReputDoesNotEvictFreshEntry(t *testing.T) {
	cache := newSignRequestCache()
	t0 := time.Unix(1_000_000, 0)
	cache.now = func() time.Time { return t0 }

	const targetKey = "target"

	// 1. Put target at t0.
	cache.put(targetKey, &IsLockAttestationResponse{TxId: targetKey})

	// 2. Advance past TTL, get() expires it (and per the R18 fix
	//    should also splice the fifo slot).
	cache.now = func() time.Time { return t0.Add(signCacheTTL + time.Second) }
	_, ok := cache.get(targetKey)
	assert.False(t, ok, "expired entry should report miss")

	// 3. Re-put target at the new time. Must end up with EXACTLY one
	//    fifo slot for it — the R18 fix's whole point.
	cache.put(targetKey, &IsLockAttestationResponse{TxId: targetKey})

	// Sanity: count fifo occurrences of targetKey. The fix is correct
	// iff this is exactly 1.
	occurrences := 0
	for _, k := range cache.fifo {
		if k == targetKey {
			occurrences++
		}
	}
	assert.Equal(t, 1, occurrences,
		"after expire-reput, target must appear in fifo exactly once "+
			"(audit R18 — the pre-fix dup-fifo bug let later evictions "+
			"destroy the live entry)")

	// Step 4 (audit R19-CONS-sign-cache-test-step4-comment-
	// misdescribes-code-and-no-eviction-triggered): the original
	// follow-up assertion tried to verify "fresh entry survives a
	// sibling eviction" by adding one sibling. With capacity=1000 +
	// only 2 entries total that branch never triggered eviction —
	// the test was a TTL check masquerading as an eviction-survival
	// check. The meaningful R18 assertion is already complete at
	// step 3: exactly ONE fifo slot for target. The eviction-
	// survival angle is hard to construct as a test that
	// distinguishes pre-R18 from post-R18 (in the natural ordering
	// both versions evict target as fifo[0]); the more meaningful
	// regression is the unbounded fifo growth covered separately by
	// TestSignCache_ExpireReputDoesNotGrowUnbounded below.
	gotResp, gotOK := cache.get(targetKey)
	require.True(t, gotOK, "freshly-reput entry must still be in cache "+
		"(within TTL of step 3's put)")
	assert.Equal(t, targetKey, gotResp.TxId)
}

// TestSignCache_ExpireReputDoesNotGrowUnbounded is the operational
// half of the same audit cluster (R18-OPS-sign-cache-fifo-grows-
// unbounded-after-ttl-reinsertion). Pre-fix: 100 expire/reput cycles
// on 50 unique keys produced fifo=5000, entries=50. Post-fix:
// fifo=50, entries=50.
func TestSignCache_ExpireReputDoesNotGrowUnbounded(t *testing.T) {
	cache := newSignRequestCache()
	tNow := time.Unix(1_000_000, 0)
	cache.now = func() time.Time { return tNow }

	const numKeys = 50
	const cycles = 100

	for cycle := 0; cycle < cycles; cycle++ {
		// Advance past TTL between cycles so each iteration is an
		// expire + reput.
		tNow = tNow.Add(signCacheTTL + time.Second)
		for i := 0; i < numKeys; i++ {
			key := "k" + hex.EncodeToString([]byte{byte(i), byte(i >> 8)})
			// get to trigger expire (after the first cycle entries are stale).
			_, _ = cache.get(key)
			cache.put(key, &IsLockAttestationResponse{TxId: key})
		}
	}

	// fifo size MUST equal numKeys (= 50) — post-fix the splice on
	// expire removes exactly one slot per re-put, so the
	// "expire + reput" cycle is fifo-neutral. The prior "numKeys+10"
	// slack was a loose bound from when this test was first written
	// against an in-progress fix; the +10 leeway was never needed.
	// Audit R19-OPS-sign-cache-fifo-slack-magic-number.
	assert.Equal(t, numKeys, len(cache.fifo),
		"fifo must stay exactly at numKeys (= live entry count); "+
			"splice on expire is fifo-neutral so cycling cannot accumulate slack")
	assert.Equal(t, numKeys, len(cache.entries),
		"entries map should stabilise at the unique-key count")
}

// TestHandleRequest_SingleFlightAcrossConcurrentRebroadcasts covers
// audit R19-OPS-handle-request-not-single-flight-concurrent-rebroadcast-
// double-signs. Two concurrent rebroadcasts of the same request must
// share ONE Sign() call — the leader signs + caches; the follower
// waits + replays the cached response. Pre-fix both threads missed
// the cache between get() and put() and both called Sign(), defeating
// the cache godoc's "exactly ONCE" claim.
func TestHandleRequest_SingleFlightAcrossConcurrentRebroadcasts(t *testing.T) {
	signer := newCountingSigner(t, "did:key:validator-1")
	// Audit R20-CORR-singleflight-test-does-not-force-race: install a
	// barrier so the FIRST Sign() blocks until the test explicitly
	// releases it. Without this, on a serializing Go scheduler the
	// leader could finish + put-into-cache before followers entered
	// handleRequest, and they'd hit the OUTER cache (R17 path) — the
	// test would pass even if singleflight itself were a no-op.
	signer.signBarrier = make(chan struct{})

	const hex32 = "0011223344556677889900112233445566778899001122334455667788990011"
	svc := NewService("vsc-testnet", signer, fakeMemoryFor(t, hex32), nil)

	req := IsLockAttestationRequest{
		TxId:               hex32,
		RawTxHashHex:       hex32,
		InstructionHashHex: hex32,
		Epoch:              1,
		ChainId:            "vsc-testnet",
	}

	const concurrency = 8
	var (
		mu   sync.Mutex
		sent []*p2pMessage
		wg   sync.WaitGroup
	)
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			svc.handleRequest(context.Background(), req, func(m p2pMessage) error {
				mu.Lock()
				mc := m
				sent = append(sent, &mc)
				mu.Unlock()
				return nil
			})
		}()
	}

	// Wait for the leader to enter Sign() (signCalls.Load() == 1) AND
	// for ALL N-1 followers to park on <-call.done (audit R21-CORR-go-
	// vsc-singleflight-test-eventually-cond-does-not-actually-prove-
	// follower-registered: the prior `len(svc.signSF.calls) == 1`
	// predicate only proved the leader had registered itself in the
	// map — it could not distinguish '1 leader, 0 followers' from
	// '1 leader, N-1 followers' because the calls map holds one entry
	// per cacheKey, not one per goroutine). waitersForKey() returns
	// the count of follower goroutines currently parked on the call's
	// done channel — only when that hits concurrency-1 do we KNOW
	// every follower is in singleflight-coordination mode (not racing
	// past the outer cache lookup). THEN release the barrier —
	// followers must replay, NOT call Sign again.
	cacheKey := signCacheKey(req)
	require.Eventually(t, func() bool {
		if signer.signCalls.Load() != 1 {
			return false
		}
		return svc.signSF.waitersForKey(cacheKey) >= int64(concurrency-1)
	}, 5*time.Second, time.Millisecond,
		"leader must enter Sign + all %d followers must park on <-call.done before release", concurrency-1)

	close(signer.signBarrier)
	wg.Wait()

	assert.Equal(t, concurrency, len(sent),
		"all %d concurrent rebroadcasts must produce a response (leader + followers replay)", concurrency)
	assert.Equal(t, int64(1), signer.signCalls.Load(),
		"single-flight: only ONE Sign() across %d concurrent identical "+
			"rebroadcasts (audit R19) — the rest must wait + replay", concurrency)
	for i, m := range sent {
		require.NotNil(t, m.Response, "response %d nil", i)
		assert.Equal(t, sent[0].Response.BlsSigHex, m.Response.BlsSigHex,
			"every follower must replay the leader's signature")
	}
}

// TestHandleRequest_SingleFlightPanicDoesNotStrand covers audit
// R20-CORR-singleflight-panic-leaks-followers-forever (HIGH).
// If the leader's Sign() panics, the singleflight cleanup MUST still
// run — followers must unblock with (nil, false), and the cacheKey
// must NOT be permanently registered in s.signSF.calls.
func TestHandleRequest_SingleFlightPanicDoesNotStrand(t *testing.T) {
	signer := newCountingSigner(t, "did:key:validator-1")
	signer.signPanic = func(_ IsLockAttestationRequest) any {
		return "synthetic Sign panic for R20 regression test"
	}

	const hex32 = "0011223344556677889900112233445566778899001122334455667788990011"
	svc := NewService("vsc-testnet", signer, fakeMemoryFor(t, hex32), nil)

	req := IsLockAttestationRequest{
		TxId:               hex32,
		RawTxHashHex:       hex32,
		InstructionHashHex: hex32,
		Epoch:              1,
		ChainId:            "vsc-testnet",
	}

	// Drive handleRequest in a goroutine + recover its panic. The
	// panic SHOULD propagate out of handleRequest (the outer caller
	// in production uses utils.RecoverGoroutine to swallow), but the
	// singleflight cleanup MUST run via defer before propagation.
	done := make(chan struct{})
	var recovered any
	go func() {
		defer close(done)
		defer func() { recovered = recover() }()
		svc.handleRequest(context.Background(), req, func(p2pMessage) error { return nil })
	}()
	<-done

	require.NotNil(t, recovered,
		"signPanic stub must produce a panic that propagates to the test goroutine")

	// The cacheKey must be GONE from the singleflight map. If the
	// pre-R20 (panic-unsafe) code path is in effect, the entry would
	// remain forever + any subsequent handleRequest with the same key
	// would deadlock as a follower.
	svc.signSF.mu.Lock()
	calls := len(svc.signSF.calls)
	svc.signSF.mu.Unlock()
	assert.Zero(t, calls,
		"singleflight map must be CLEAN after a panic-during-Sign — "+
			"audit R20-CORR-singleflight-panic-leaks-followers-forever; "+
			"pre-fix the cacheKey would stay registered + every future "+
			"follower would block on a never-closed done channel")

	// And a fresh request with the same key (after clearing the panic
	// stub) must complete cleanly — proves we didn't leave the cache
	// poisoned. Audit R21-CONS-singleflight-panic-test-comment-uses-
	// eventually-but-code-does-not: the prior implementation used a
	// fixed 2s deadline; the comment promised Eventually. require.
	// Eventually polls every 10ms up to 2s so a slow CI runner
	// surfaces "still alive" rather than spuriously failing on a
	// scheduler stall, AND a real deadlock fails fast on the next
	// poll instead of hanging the test runner.
	signer.signPanic = nil
	freshDone := make(chan struct{})
	go func() {
		defer close(freshDone)
		svc.handleRequest(context.Background(), req, func(p2pMessage) error { return nil })
	}()
	require.Eventually(t, func() bool {
		select {
		case <-freshDone:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond,
		"post-panic handleRequest deadlocked — singleflight cleanup failed")
}

// TestHandleRequest_SingleFlightPanicWakesWaitingFollowers covers
// audit R21-OPS-singleflight-panic-test-does-not-cover-follower-
// already-waiting-during-leader-panic (MEDIUM) — the harder case of
// the R20 panic-safety regression. The sibling
// TestHandleRequest_SingleFlightPanicDoesNotStrand only exercises
// SEQUENTIAL leader-panic-then-retry; it does NOT exercise the exact
// pre-R20 production failure mode where N followers are ALREADY
// parked on <-call.done when the leader panics. Pre-R20 those
// followers would block on a never-closed done channel forever. The
// R20 defer { delete; close } sequence MUST wake them all.
//
// Design: install signBarrier so the leader stops inside Sign() —
// followers register on signSF and park on <-call.done. Use
// waitersForKey() to wait until ALL N-1 followers are parked (audit
// R21-CORR-singleflight-test-eventually-cond-does-not-actually-
// prove-follower-registered: previously the test only proved the
// leader had registered itself, not that followers were parked).
// THEN close the barrier so the leader unblocks + hits signPanic.
// The R20 defer runs → call.done closes → all N-1 followers MUST
// wake within a bounded time and the singleflight map MUST be empty.
func TestHandleRequest_SingleFlightPanicWakesWaitingFollowers(t *testing.T) {
	signer := newCountingSigner(t, "did:key:validator-1")
	signer.signBarrier = make(chan struct{})
	signer.signPanic = func(_ IsLockAttestationRequest) any {
		return "synthetic Sign panic with followers parked (R21)"
	}

	const hex32 = "0011223344556677889900112233445566778899001122334455667788990011"
	svc := NewService("vsc-testnet", signer, fakeMemoryFor(t, hex32), nil)

	req := IsLockAttestationRequest{
		TxId:               hex32,
		RawTxHashHex:       hex32,
		InstructionHashHex: hex32,
		Epoch:              1,
		ChainId:            "vsc-testnet",
	}

	const concurrency = 4
	var (
		wg        sync.WaitGroup
		recovered atomic.Int64
	)
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				// One goroutine — the leader — will see a panic
				// propagate out of svc.handleRequest. The N-1 followers
				// return cleanly with (nil, false) from singleflight.do
				// and handleRequest silently drops (no panic).
				if r := recover(); r != nil {
					recovered.Add(1)
				}
			}()
			svc.handleRequest(context.Background(), req, func(p2pMessage) error { return nil })
		}()
	}

	cacheKey := signCacheKey(req)
	require.Eventually(t, func() bool {
		if signer.signCalls.Load() != 1 {
			return false
		}
		return svc.signSF.waitersForKey(cacheKey) >= int64(concurrency-1)
	}, 5*time.Second, time.Millisecond,
		"leader must enter Sign + all %d followers must park before barrier release", concurrency-1)

	close(signer.signBarrier)

	// All N goroutines MUST complete within a bounded time. Pre-R20
	// the followers would be parked on a never-closed done channel +
	// this test would deadlock here. Post-R20 the leader's defer
	// closes call.done as part of cleanup → followers wake with
	// resp==nil and return.
	doneAll := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneAll)
	}()
	select {
	case <-doneAll:
	case <-time.After(3 * time.Second):
		t.Fatal("panic-during-Sign stranded waiting followers — " +
			"singleflight.do defer-close failed (audit R21-OPS-singleflight-" +
			"panic-test-does-not-cover-follower-already-waiting-during-" +
			"leader-panic; pre-R20 regression mode)")
	}

	assert.Equal(t, int64(1), recovered.Load(),
		"exactly the leader goroutine should recover a panic; %d followers "+
			"complete normally via (nil, false) from singleflight.do", concurrency-1)

	svc.signSF.mu.Lock()
	calls := len(svc.signSF.calls)
	svc.signSF.mu.Unlock()
	assert.Zero(t, calls,
		"singleflight map must be CLEAN after leader-panic-with-N-followers-"+
			"already-parked — audit R20-CORR-singleflight-panic-leaks-"+
			"followers-forever + R21-OPS-singleflight-panic-test-does-"+
			"not-cover-follower-already-waiting-during-leader-panic")
}

// dualMemory returns rawTxHash=a for txA and =b for txB. Used by the
// no-dedupe test where we need two distinct observed txids.
type dualMemory struct {
	a, b     []byte
	txA, txB string
}

func (d *dualMemory) Lookup(txid string) (string, []byte, bool) {
	switch txid {
	case d.txA:
		return "deadbeef", d.a, true
	case d.txB:
		return "deadbeef", d.b, true
	}
	return "", nil, false
}
