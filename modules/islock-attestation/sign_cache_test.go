package islock_attestation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	bls "github.com/protolambda/bls12-381-util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingSigner wraps a real BLS signer + records how many times
// Sign() was called. Used to prove the cache short-circuits Sign() on
// rebroadcast.
type countingSigner struct {
	inner     *fakeSigner
	signCalls int
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
	c.signCalls++
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
	assert.Equal(t, 1, signer.signCalls,
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

	assert.Equal(t, 2, signer.signCalls,
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
	wg.Wait()

	assert.Equal(t, concurrency, len(sent),
		"all %d concurrent rebroadcasts must produce a response (leader + followers replay)", concurrency)
	assert.Equal(t, 1, signer.signCalls,
		"single-flight: only ONE Sign() across %d concurrent identical "+
			"rebroadcasts (audit R19) — the rest must wait + replay", concurrency)
	for i, m := range sent {
		require.NotNil(t, m.Response, "response %d nil", i)
		assert.Equal(t, sent[0].Response.BlsSigHex, m.Response.BlsSigHex,
			"every follower must replay the leader's signature")
	}
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
