package islock_attestation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	bls "github.com/protolambda/bls12-381-util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSigner implements AttestationSigner with a real BLS key + pubkey.
// Round-2 audit R2-001 regression guard: handleRequest must populate
// PubkeyHex from the signer; without this the producer/consumer
// mismatch in the broadcaster + contract silently drops every response.
type fakeSigner struct {
	did   string
	sk    *bls.SecretKey
	pkHex string
}

func newFakeSigner(t *testing.T, did string) *fakeSigner {
	t.Helper()
	var skBytes [32]byte
	_, err := rand.Read(skBytes[:])
	require.NoError(t, err)
	var sk bls.SecretKey
	require.NoError(t, sk.Deserialize(&skBytes))
	pk, err := bls.SkToPk(&sk)
	require.NoError(t, err)
	pkBytes := pk.Serialize()
	return &fakeSigner{did: did, sk: &sk, pkHex: hex.EncodeToString(pkBytes[:])}
}

func (f *fakeSigner) ValidatorDID() string { return f.did }
func (f *fakeSigner) PubkeyHex() string    { return f.pkHex }
func (f *fakeSigner) Sign(req IsLockAttestationRequest) (string, error) {
	return Sign(req, f.sk)
}

// fakeMemory always returns "found" with the configured rawTxHash bytes.
// The rawTxHash MUST match the internal byte order of req.RawTxHashHex
// (i.e. reverse of the display-order wire hex) after SEC-5 (R15) added
// the local-observation cross-check in handleRequest.
type fakeMemory struct {
	rawTxHash []byte
}

func (f fakeMemory) Lookup(txid string) (string, []byte, bool) {
	return "deadbeef", f.rawTxHash, true
}

// fakeMemoryFor builds a fakeMemory whose stored rawTxHash matches
// the SEC-5 check for req.RawTxHashHex = displayHex (32 hex bytes,
// 64 chars). Internal-order = reverse of display.
func fakeMemoryFor(t *testing.T, displayHex string) fakeMemory {
	t.Helper()
	display, err := hex.DecodeString(displayHex)
	require.NoError(t, err)
	require.Len(t, display, 32)
	return fakeMemory{rawTxHash: ReverseBytesCopy(display)}
}

func TestHandleRequest_PopulatesPubkeyHex(t *testing.T) {
	signer := newFakeSigner(t, "did:key:validator-1")
	// Build a valid request — 32-byte hex txid/rawTxHash/instructionHash.
	const hex32 = "0011223344556677889900112233445566778899001122334455667788990011"
	svc := NewService("vsc-testnet", signer, fakeMemoryFor(t, hex32), nil)
	req := IsLockAttestationRequest{
		TxId:               hex32,
		RawTxHashHex:       hex32,
		InstructionHashHex: hex32,
		Epoch:              1,
		ChainId:            "vsc-testnet",
	}

	var sent *p2pMessage
	send := func(m p2pMessage) error {
		sent = &m
		return nil
	}
	svc.handleRequest(context.Background(), req, send)
	require.NotNil(t, sent, "handleRequest must call send")
	require.Equal(t, "response", sent.Type)
	require.NotNil(t, sent.Response)
	r := sent.Response

	assert.Equal(t, "did:key:validator-1", r.ValidatorDID)
	assert.Equal(t, signer.pkHex, r.PubkeyHex,
		"PubkeyHex MUST be populated from signer — broadcaster + contract reject empty")
	assert.Len(t, r.PubkeyHex, 96, "PubkeyHex is 48 bytes (96 hex chars)")
	assert.Len(t, r.BlsSigHex, 192, "BlsSigHex is 96 bytes (192 hex chars)")
}

func TestHandleRequest_NilSignerSilentlyIgnores(t *testing.T) {
	// IS-service-side: no signer, no memory. handleRequest must early-
	// return without sending.
	svc := NewService("vsc-testnet", nil, nil, nil)

	const hex32 = "0011223344556677889900112233445566778899001122334455667788990011"
	req := IsLockAttestationRequest{
		TxId:               hex32,
		RawTxHashHex:       hex32,
		InstructionHashHex: hex32,
		Epoch:              1,
		ChainId:            "vsc-testnet",
	}

	called := false
	send := func(m p2pMessage) error {
		called = true
		return nil
	}
	svc.handleRequest(context.Background(), req, send)
	assert.False(t, called, "IS-service-side handleRequest must not send")
}

// TestHandleRequest_RejectsMismatchedRawTxHash covers audit SEC-5 (R15):
// the validator must verify req.RawTxHashHex matches the locally-
// observed sha256d of the tx body before signing. Without this gate the
// validator would sign whatever hash the requester supplied — burning
// CPU on contract-rejected forgeries AND becoming a forgery primitive
// if a future contract change relaxes its own RawTxHashHex recompute.
func TestHandleRequest_RejectsMismatchedRawTxHash(t *testing.T) {
	signer := newFakeSigner(t, "did:key:validator-1")

	// fakeMemory observation is the displayHex below. The request
	// claims a DIFFERENT rawTxHash (otherHex). With SEC-5 in place the
	// validator must NOT send.
	const observedDisplayHex = "0011223344556677889900112233445566778899001122334455667788990011"
	const otherDisplayHex = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	svc := NewService("vsc-testnet", signer, fakeMemoryFor(t, observedDisplayHex), nil)

	req := IsLockAttestationRequest{
		TxId:               observedDisplayHex,
		RawTxHashHex:       otherDisplayHex, // requester-supplied, NOT what we saw
		InstructionHashHex: observedDisplayHex,
		Epoch:              1,
		ChainId:            "vsc-testnet",
	}

	called := false
	send := func(m p2pMessage) error {
		called = true
		return nil
	}
	svc.handleRequest(context.Background(), req, send)
	assert.False(t, called,
		"validator must drop the request silently when req.RawTxHashHex "+
			"does not match the locally-observed sha256d — SEC-5")
}

// TestHandleRequest_RejectsMalformedRawTxHash guards the parse step in
// the SEC-5 check. A non-32-byte hex or a hex-decode error must drop,
// not panic.
func TestHandleRequest_RejectsMalformedRawTxHash(t *testing.T) {
	signer := newFakeSigner(t, "did:key:validator-1")
	const hex32 = "0011223344556677889900112233445566778899001122334455667788990011"
	svc := NewService("vsc-testnet", signer, fakeMemoryFor(t, hex32), nil)

	cases := []struct {
		name string
		bad  string
	}{
		{"non-hex", "not-a-hex-string"},
		{"wrong length", "0011223344"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := IsLockAttestationRequest{
				TxId:               hex32,
				RawTxHashHex:       tc.bad,
				InstructionHashHex: hex32,
				Epoch:              1,
				ChainId:            "vsc-testnet",
			}
			called := false
			send := func(m p2pMessage) error {
				called = true
				return nil
			}
			svc.handleRequest(context.Background(), req, send)
			assert.False(t, called,
				"malformed RawTxHashHex must drop silently, not sign")
		})
	}
}
