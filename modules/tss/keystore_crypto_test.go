package tss

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	datastore "github.com/ipfs/go-datastore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// review2 CRITICAL #4 — TSS key shares were persisted to the flatfs keystore
// as plaintext json.Marshal output. These tests pin the encrypt-at-rest
// helpers: AES-256-GCM with a key derived from the node's BLS seed, plus a
// legacy-plaintext passthrough so existing on-disk shares still load and get
// transparently re-encrypted on the next write.

func validSeedHex(t *testing.T) string {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return hex.EncodeToString(seed)
}

func TestDeriveKeystoreKey_DeterministicAnd32Bytes(t *testing.T) {
	seed := validSeedHex(t)

	k1, err := deriveKeystoreKey(seed)
	require.NoError(t, err)
	require.Len(t, k1, 32)

	k2, err := deriveKeystoreKey(seed)
	require.NoError(t, err)
	assert.Equal(t, k1, k2, "derivation must be deterministic across restarts")

	// A different seed must yield a different key.
	other := make([]byte, 32)
	other[0] = 0xff
	k3, err := deriveKeystoreKey(hex.EncodeToString(other))
	require.NoError(t, err)
	assert.NotEqual(t, k1, k3)
}

func TestDeriveKeystoreKey_RejectsBadSeed(t *testing.T) {
	_, err := deriveKeystoreKey("not-hex")
	assert.Error(t, err)

	_, err = deriveKeystoreKey(hex.EncodeToString([]byte("too-short")))
	assert.Error(t, err, "seed shorter than 32 bytes must be rejected")
}

func TestKeystoreBlob_RoundTrip(t *testing.T) {
	key, err := deriveKeystoreKey(validSeedHex(t))
	require.NoError(t, err)

	plaintext := []byte(`{"Xi":"super-secret-ecdsa-share","PaillierSK":{"N":"617-digit-number"}}`)

	enc, err := encryptKeystoreBlob(key, plaintext)
	require.NoError(t, err)

	// The persisted blob must not be the plaintext, and must not contain
	// recognizable secret material.
	assert.NotEqual(t, plaintext, enc)
	assert.False(t, bytes.Contains(enc, []byte("super-secret-ecdsa-share")),
		"ciphertext leaks the private share")

	dec, err := decryptKeystoreBlob(key, enc)
	require.NoError(t, err)
	assert.Equal(t, plaintext, dec)
}

func TestKeystoreBlob_LegacyPlaintextPassthrough(t *testing.T) {
	key, err := deriveKeystoreKey(validSeedHex(t))
	require.NoError(t, err)

	// An existing on-disk share is raw JSON with no VSC encryption envelope.
	legacy := []byte(`{"Xi":"legacy-plaintext-share"}`)

	dec, err := decryptKeystoreBlob(key, legacy)
	require.NoError(t, err, "must still load pre-existing plaintext key files")
	assert.Equal(t, legacy, dec)
}

func TestKeystoreBlob_WrongKeyFails(t *testing.T) {
	key, err := deriveKeystoreKey(validSeedHex(t))
	require.NoError(t, err)

	enc, err := encryptKeystoreBlob(key, []byte("the share"))
	require.NoError(t, err)

	wrong := make([]byte, 32)
	wrong[0] = 0xaa
	_, err = decryptKeystoreBlob(wrong, enc)
	assert.Error(t, err, "GCM auth must reject a wrong key / tampered blob")
}

func TestKeystoreBlob_NonceIsRandom(t *testing.T) {
	key, err := deriveKeystoreKey(validSeedHex(t))
	require.NoError(t, err)

	pt := []byte("same plaintext")
	a, err := encryptKeystoreBlob(key, pt)
	require.NoError(t, err)
	b, err := encryptKeystoreBlob(key, pt)
	require.NoError(t, err)

	assert.NotEqual(t, a, b, "each encryption must use a fresh nonce")

	da, err := decryptKeystoreBlob(key, a)
	require.NoError(t, err)
	db, err := decryptKeystoreBlob(key, b)
	require.NoError(t, err)
	assert.Equal(t, pt, da)
	assert.Equal(t, pt, db)
}

// TestKeystoreWrappers_OnDiskCiphertextAndLegacyMigration exercises the exact
// wrappers every dispatcher.go site now uses: what lands in the datastore
// must be ciphertext, it must round-trip, and a pre-existing plaintext entry
// (a node upgraded in place) must still load.
func TestKeystoreWrappers_OnDiskCiphertextAndLegacyMigration(t *testing.T) {
	key, err := deriveKeystoreKey(validSeedHex(t))
	require.NoError(t, err)

	ds := datastore.NewMapDatastore()
	ctx := context.Background()
	k := datastore.NewKey("key-abc-0")

	share := []byte(`{"Xi":"ecdsa-private-share","PaillierSK":{"P":"prime-p","Q":"prime-q"}}`)

	require.NoError(t, keystorePutEncrypted(ctx, ds, k, share, key))

	// What is actually persisted must be the encrypted envelope, never the
	// secret in the clear.
	stored, err := ds.Get(ctx, k)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(stored, keystoreEncMagic), "stored blob is not encrypted")
	assert.False(t, bytes.Contains(stored, []byte("ecdsa-private-share")), "secret persisted in clear")

	got, err := keystoreGetDecrypted(ctx, ds, k, key)
	require.NoError(t, err)
	assert.Equal(t, share, got)

	// Legacy: a plaintext entry written by an older binary still loads.
	legacyKey := datastore.NewKey("key-legacy-0")
	legacy := []byte(`{"Xi":"old-plaintext-share"}`)
	require.NoError(t, ds.Put(ctx, legacyKey, legacy))

	got, err = keystoreGetDecrypted(ctx, ds, legacyKey, key)
	require.NoError(t, err)
	assert.Equal(t, legacy, got, "pre-existing plaintext share must still load")
}
