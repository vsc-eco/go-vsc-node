package chain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignatureChannels_MakeSession(t *testing.T) {
	sc := makeSignatureChannels()

	ch, err := sc.makeSession("test-session")
	require.NoError(t, err)
	assert.NotNil(t, ch)
}

func TestSignatureChannels_DuplicateSession(t *testing.T) {
	sc := makeSignatureChannels()

	_, err := sc.makeSession("test-session")
	require.NoError(t, err)

	_, err = sc.makeSession("test-session")
	assert.ErrorIs(t, err, errChannelExists)
}

func TestSignatureChannels_ReceiveSignature(t *testing.T) {
	sc := makeSignatureChannels()

	ch, err := sc.makeSession("sess-1")
	require.NoError(t, err)

	msg := signatureMessage{
		Signature: "test-sig-base64",
		Account:   "witness1",
		BlsDid:    "did:key:z6test",
	}

	err = sc.receiveSignature("sess-1", msg)
	require.NoError(t, err)

	received := <-ch
	assert.Equal(t, "test-sig-base64", received.Signature)
	assert.Equal(t, "witness1", received.Account)
	assert.Equal(t, "did:key:z6test", received.BlsDid)
}

func TestSignatureChannels_ReceiveInvalidSession(t *testing.T) {
	sc := makeSignatureChannels()

	err := sc.receiveSignature("nonexistent", signatureMessage{})
	assert.ErrorIs(t, err, errInvalidSession)
}

func TestSignatureChannels_ChannelFull(t *testing.T) {
	sc := makeSignatureChannels()

	_, err := sc.makeSession("sess-full")
	require.NoError(t, err)

	// Fill the channel (buffer size is 8)
	for i := 0; i < 8; i++ {
		err = sc.receiveSignature("sess-full", signatureMessage{
			Account: "witness",
		})
		require.NoError(t, err)
	}

	// 9th should fail
	err = sc.receiveSignature("sess-full", signatureMessage{
		Account: "overflow",
	})
	assert.ErrorIs(t, err, errChannelFull)
}

func TestSignatureChannels_ClearSession(t *testing.T) {
	sc := makeSignatureChannels()

	_, err := sc.makeSession("sess-clear")
	require.NoError(t, err)

	sc.clearSession("sess-clear")

	// Should be able to create a new session with same ID
	_, err = sc.makeSession("sess-clear")
	assert.NoError(t, err)

	// Clearing a nonexistent session should not panic
	sc.clearSession("nonexistent")
}

func TestSignatureChannels_ClearMap(t *testing.T) {
	sc := makeSignatureChannels()

	_, err := sc.makeSession("sess-a")
	require.NoError(t, err)
	_, err = sc.makeSession("sess-b")
	require.NoError(t, err)

	sc.clearMap()

	// Should be able to recreate sessions
	_, err = sc.makeSession("sess-a")
	assert.NoError(t, err)
}

// NC-1: a junk message that claims a victim's DID (unverified at intake) must NOT block the
// victim's genuine signature. The intake dedup keys on the (DID, signature) pair, and the
// genuine signature differs from the attacker's forged one, so both enqueue — the collector
// then rejects the junk post-verify (AddAndVerify) and credits only the genuine signature.
func TestReceiveSignature_ForgedDidDoesNotBlockGenuineSignature(t *testing.T) {
	sc := makeSignatureChannels()
	ch, err := sc.makeSession("nc1")
	require.NoError(t, err)

	// Attacker pre-sends a junk response carrying the VICTIM's DID with a bogus signature.
	require.NoError(t, sc.receiveSignature("nc1", signatureMessage{
		BlsDid: "did:key:victim", Signature: "forged-garbage", Account: "attacker",
	}), "the junk message itself is accepted at intake (it is only rejected on verify)")

	// The victim's GENUINE signature (same DID, a DIFFERENT real signature) must still enqueue.
	require.NoError(t, sc.receiveSignature("nc1", signatureMessage{
		BlsDid: "did:key:victim", Signature: "genuine-sig", Account: "victim",
	}), "a junk message claiming the victim's DID must not block the victim's genuine signature (NC-1)")

	require.Equal(t, 2, len(ch),
		"both the junk and the genuine message enqueue; the collector rejects the junk post-verify")

	// An exact identical repeat of the genuine message is still deduped (VR2-19).
	require.ErrorIs(t, sc.receiveSignature("nc1", signatureMessage{
		BlsDid: "did:key:victim", Signature: "genuine-sig", Account: "victim",
	}), errDuplicateSig, "an identical (DID, signature) repeat must still be dropped at intake")
}
