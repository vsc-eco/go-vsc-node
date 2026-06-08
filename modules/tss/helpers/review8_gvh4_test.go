package tss_helpers

// review8 GV-H4 — the TSS signing primitive silently truncated oversized
// digests, so two different messages sharing a 32-byte prefix are signed to the
// SAME scalar (one threshold signature valid for both). hashToInt does
// `if len(hash) > orderBytes { hash = hash[:orderBytes] }`.
//
// The correct fix is NOT to re-hash inside the primitive: the ECDSA signature
// must verify on an external chain (BTC/ETH) over the EXACT 32-byte sighash.
// The safe fix is to REJECT a malformed (non-32-byte) digest instead of
// truncating it.

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

func TestReview8_GVH4_RejectsTruncatableDigest(t *testing.T) {
	prefix := bytes.Repeat([]byte{0xab}, 32)

	// Mechanism: hashToInt truncates, so two distinct >32-byte messages sharing
	// the first 32 bytes collide to one signed scalar.
	msg1 := append(append([]byte{}, prefix...), 0x01)
	msg2 := append(append([]byte{}, prefix...), 0x02)
	require.NotEqual(t, msg1, msg2)
	require.Equal(t, 0,
		hashToInt(msg1, btcec.S256()).Cmp(hashToInt(msg2, btcec.S256())),
		"hashToInt truncates >32B inputs to a colliding scalar (the GV-H4 primitive)")

	// Fix: MsgToHashInt must REJECT non-32-byte ECDSA digests, not truncate.
	_, err1 := MsgToHashInt(msg1, SigningAlgoEcdsa)
	_, err2 := MsgToHashInt(msg2, SigningAlgoEcdsa)
	require.Error(t, err1, "oversized digest must be rejected")
	require.Error(t, err2, "oversized digest must be rejected")
	_, errShort := MsgToHashInt(prefix[:31], SigningAlgoEcdsa)
	require.Error(t, errShort, "undersized digest must be rejected")

	// A well-formed 32-byte sighash is unchanged — signed verbatim.
	got, err := MsgToHashInt(prefix, SigningAlgoEcdsa)
	require.NoError(t, err)
	require.Equal(t, 0, hashToInt(prefix, btcec.S256()).Cmp(got),
		"valid 32-byte digest must sign to the same scalar as before (no external breakage)")
}
