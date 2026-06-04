package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddressSignerEd25519_RoundTrip(t *testing.T) {
	// Deterministic seed so verification + expected pubkey are stable.
	seedHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "signer.key")
	require.NoError(t, os.WriteFile(keyPath, []byte(seedHex), 0o600))

	signer, pubHex, err := NewAddressSignerEd25519FromFile(keyPath)
	require.NoError(t, err)
	require.NotEmpty(t, pubHex)
	require.Len(t, pubHex, 64, "Ed25519 pubkey is 32 bytes → 64 hex chars")

	depositAddr := "8jU46MxM2TpUsFFpm7V4fjJ4F9uroyTir1"
	instruction := "op=auth;sid=abc123"
	sigB64, err := signer.Sign(context.Background(), depositAddr, instruction)
	require.NoError(t, err)
	require.NotEmpty(t, sigB64)

	sig, err := base64.StdEncoding.DecodeString(sigB64)
	require.NoError(t, err)
	require.Len(t, sig, ed25519.SignatureSize, "Ed25519 sig is 64 bytes")

	pub, err := hex.DecodeString(pubHex)
	require.NoError(t, err)

	// The canonical message format MUST stay in lockstep with the HMAC
	// stub (depositAddr || 0x00 || instruction) so Altera's signer-
	// kind-agnostic verification path keeps working.
	msg := append([]byte(depositAddr), 0)
	msg = append(msg, []byte(instruction)...)
	assert.True(t, ed25519.Verify(pub, msg, sig),
		"Ed25519 verify must round-trip against the canonical message bytes")
}

func TestAddressSignerEd25519_RefusesPermissiveKeyFile(t *testing.T) {
	seedHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "signer.key")
	require.NoError(t, os.WriteFile(keyPath, []byte(seedHex), 0o644))

	_, _, err := NewAddressSignerEd25519FromFile(keyPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "world/group-readable",
		"loading a key file with 0o644 perms must refuse with a clear message")
}

func TestAddressSignerEd25519_RejectsMalformedSeed(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "signer.key")
	require.NoError(t, os.WriteFile(keyPath, []byte("not-hex"), 0o600))

	_, _, err := NewAddressSignerEd25519FromFile(keyPath)
	require.Error(t, err)
}

func TestAddressSignerEd25519_SameMessageProducesSameSignature(t *testing.T) {
	// Ed25519 is deterministic per RFC 8032 — same key + same message
	// MUST yield the same sig. Critical so test-vector fixtures are
	// stable across is-service restarts.
	seedHex := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "signer.key")
	require.NoError(t, os.WriteFile(keyPath, []byte(seedHex), 0o600))

	signer, _, err := NewAddressSignerEd25519FromFile(keyPath)
	require.NoError(t, err)
	a, _ := signer.Sign(context.Background(), "addr1", "instr1")
	b, _ := signer.Sign(context.Background(), "addr1", "instr1")
	assert.Equal(t, a, b, "Ed25519 sig MUST be deterministic")
}
