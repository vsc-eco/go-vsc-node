package islock_attestation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
