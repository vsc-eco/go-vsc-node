package peerpop

import (
	"errors"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func key(t *testing.T) (crypto.PrivKey, string) {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(nil)
	require.NoError(t, err)
	pid, err := peer.IDFromPrivateKey(priv)
	require.NoError(t, err)
	return priv, pid.String()
}

func TestPeerPoP_RoundTrip(t *testing.T) {
	priv, pid := key(t)
	pop, err := Sign(priv, "magi.test1", pid)
	require.NoError(t, err)
	assert.NoError(t, Verify("magi.test1", pid, pop))
}

// The squat B1 is about: another account announces the member's peer id. It holds no
// key for it, so any proof it supplies fails; reusing the member's own proof fails
// because the account is part of the signed message.
func TestPeerPoP_SquatterCannotClaimAnotherMembersPeerId(t *testing.T) {
	victim, victimPid := key(t)
	squatter, _ := key(t)
	victimPoP, err := Sign(victim, "magi.test1", victimPid)
	require.NoError(t, err)

	assert.True(t, errors.Is(Verify("squatter", victimPid, victimPoP), ErrInvalid), "replayed under another account")
	_, err = Sign(squatter, "squatter", victimPid)
	assert.Error(t, err, "a key cannot sign for a peer id that is not its own")
	assert.True(t, errors.Is(Verify("squatter", victimPid, ""), ErrMissing))
	assert.True(t, errors.Is(Verify("squatter", victimPid, "00"), ErrInvalid))
	assert.True(t, errors.Is(Verify("squatter", victimPid, "zz"), ErrInvalid))
}

func TestPeerPoP_BoundToThePeerId(t *testing.T) {
	priv, pid := key(t)
	_, otherPid := key(t)
	pop, err := Sign(priv, "magi.test1", pid)
	require.NoError(t, err)
	assert.Error(t, Verify("magi.test1", otherPid, pop))
	assert.Error(t, Verify("magi.test1", "not-a-peer-id", pop))
}
