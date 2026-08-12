package dids_test

import (
	"crypto/rand"
	"testing"

	"vsc-node/lib/dids"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ethBls "github.com/protolambda/bls12-381-util"
	kbls "github.com/kilic/bls12-381"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===== pubkey cache tests =====

// TestPubkeyCacheHitReturnsEqualIndependentKeys verifies Identifier() serves
// equal-but-independent copies from the LRU: mutating one result must never
// leak into a later call.
func TestPubkeyCacheHitReturnsEqualIndependentKeys(t *testing.T) {
	var seed [32]byte
	copy(seed[:], []byte("cache_seed_1_xxxxxxxxxxxx"))
	did, _, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed)
	require.NoError(t, err)

	k1 := did.Identifier()
	k2 := did.Identifier()
	require.NotNil(t, k1)
	require.NotNil(t, k2)

	// Same key material, independent instances.
	assert.Equal(t, k1.Serialize(), k2.Serialize())
	assert.NotSame(t, k1, k2)

	// Mutating the returned copy must not corrupt the cache: overwrite k1 with
	// a DIFFERENT key, then a fresh Identifier must still return the original.
	var seed2 [32]byte
	copy(seed2[:], []byte("cache_seed_2_xxxxxxxxxxxx"))
	did2, _, err2 := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed2)
	require.NoError(t, err2)
	other := did2.Identifier()
	require.NotNil(t, other)
	serializedOther := other.Serialize()
	require.NoError(t, k1.Deserialize(&serializedOther))

	k3 := did.Identifier()
	require.NotNil(t, k3)
	assert.Equal(t, k2.Serialize(), k3.Serialize())
}

// TestPubkeyCacheMalformedNotCached verifies malformed DIDs keep the nil
// return path (they are never stored in the cache).
func TestPubkeyCacheMalformedNotCached(t *testing.T) {
	for _, in := range []string{
		"did:key:z",
		"did:key:z6Mk",
		"did:key:z6Mkabcdefghijklmnopqrstuvwxyz1234567890ABCDEFGHIJ",
		"not-a-did",
	} {
		if pk := dids.BlsDID(in).Identifier(); pk != nil {
			t.Fatalf("BlsDID(%q).Identifier() = %v, want nil for malformed DID", in, pk)
		}
		// second call exercises the (empty) cache path identically
		if pk := dids.BlsDID(in).Identifier(); pk != nil {
			t.Fatalf("BlsDID(%q).Identifier() (2nd) = %v, want nil", in, pk)
		}
	}
}

// ===== benchmarks =====

// benchCircuit builds a signed, serialized BLS circuit over `members` keys.
func benchCircuit(b *testing.B, members int) (dids.SerializedCircuit, []dids.BlsDID, cid.Cid) {
	b.Helper()
	msg := blocks.NewBlock([]byte("bls benchmark block")).Cid()
	keyset := make([]dids.BlsDID, 0, members)
	providers := make([]dids.BlsProvider, 0, members)
	for i := 0; i < members; i++ {
		var seed [32]byte
		if _, err := rand.Read(seed[:]); err != nil {
			b.Fatal(err)
		}
		did, privKey, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed)
		if err != nil {
			b.Fatal(err)
		}
		provider, err := dids.NewBlsProvider(privKey)
		if err != nil {
			b.Fatal(err)
		}
		keyset = append(keyset, did)
		providers = append(providers, provider)
	}

	generator := dids.NewBlsCircuitGenerator([]dids.Member(keyset))
	partial, err := generator.Generate(msg)
	if err != nil {
		b.Fatal(err)
	}
	for i, p := range providers {
		sig, err := p.Sign(msg)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := partial.AddAndVerify(keyset[i], sig); err != nil {
			b.Fatal(err)
		}
	}
	final, err := partial.Finalize()
	if err != nil {
		b.Fatal(err)
	}
	sc, err := final.Serialize()
	if err != nil {
		b.Fatal(err)
	}
	return *sc, keyset, msg
}

// BenchmarkPubkeyDeserializeCold is the full (uncached) DID→pubkey path:
// base58 decode + G1 point decompression.
func BenchmarkPubkeyDeserializeCold(b *testing.B) {
	var seed [32]byte
	copy(seed[:], []byte("bench_cold_seed_000000000"))
	did, _, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := dids.ParseBlsDID(string(did)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPubkeyDeserializeWarm is the cached path after the first lookup.
func BenchmarkPubkeyDeserializeWarm(b *testing.B) {
	var seed [32]byte
	copy(seed[:], []byte("bench_warm_seed_000000000"))
	did, _, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed)
	if err != nil {
		b.Fatal(err)
	}
	if did.Identifier() == nil {
		b.Fatal("failed to warm cache")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if did.Identifier() == nil {
			b.Fatal("unexpected nil")
		}
	}
}

// BenchmarkHashToCurve isolates the message→G2 map cost inside Verify.
func BenchmarkHashToCurve(b *testing.B) {
	msg := []byte("benchmark hash-to-curve message")
	g2 := kbls.NewG2()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g2.HashToCurve(msg, []byte("BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_POP_")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCircuitVerify is the full produce_block validation path:
// DeserializeBlsCircuit + Verify (key deserialization + hash-to-curve + 2
// pairings) over a committee-sized keyset.
func BenchmarkCircuitVerify(b *testing.B) {
	sc, keyset, msg := benchCircuit(b, 9)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		circuit, err := dids.DeserializeBlsCircuit(sc, keyset, msg)
		if err != nil {
			b.Fatal(err)
		}
		ok, _, err := circuit.Verify()
		if err != nil || !ok {
			b.Fatalf("verify failed: %v", err)
		}
	}
}

// BenchmarkSingleVerify measures the raw bls.Verify (hash-to-curve + 2
// pairings) on a single key — the floor cost per verification.
func BenchmarkSingleVerify(b *testing.B) {
	var seed [32]byte
	copy(seed[:], []byte("bench_verify_seed_00000000"))
	did, privKey, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed)
	if err != nil {
		b.Fatal(err)
	}
	msg := []byte("benchmark verify message")
	sig := ethBls.Sign(privKey, msg)
	pubKey := did.Identifier()
	if pubKey == nil {
		b.Fatal("nil pubkey")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !ethBls.Verify(pubKey, msg, sig) {
			b.Fatal("verify failed")
		}
	}
}
