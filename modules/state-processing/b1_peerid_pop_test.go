package state_engine

import (
	"errors"
	"testing"

	"vsc-node/lib/peerpop"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

// B1: from 0.12.0 a witness announce that carries a peer_id must prove the announcer
// holds that peer's libp2p key; below it every announce is stored as before.
func TestB1_WitnessAnnouncePeerIdProof(t *testing.T) {
	priv, _, err := crypto.GenerateEd25519Key(nil)
	require.NoError(t, err)
	pid, err := peer.IDFromPrivateKey(priv)
	require.NoError(t, err)
	pop, err := peerpop.Sign(priv, "magi.test1", pid.String())
	require.NoError(t, err)
	meta := func(peerId, p string) witnesses.PostingJsonMetadata {
		m := witnesses.PostingJsonMetadata{Services: []string{"vsc.network"}}
		m.VscNode.PeerId = peerId
		m.VscNode.PeerIdPoP = p
		return m
	}
	before, after := consensusversion.V0_11_0, consensusversion.V0_12_0

	ok, err := witnessAnnouncePeerIdProof(before, meta(pid.String(), ""), "squatter")
	require.NoError(t, err, "below 0.12.0: stored as before, proof or not")
	require.False(t, ok)

	ok, err = witnessAnnouncePeerIdProof(after, meta(pid.String(), pop), "magi.test1")
	require.NoError(t, err)
	require.True(t, ok, "the member's own proof verifies")

	_, err = witnessAnnouncePeerIdProof(after, meta(pid.String(), ""), "squatter")
	require.True(t, errors.Is(err, peerpop.ErrMissing), "a squat without a proof is rejected")
	_, err = witnessAnnouncePeerIdProof(after, meta(pid.String(), pop), "squatter")
	require.True(t, errors.Is(err, peerpop.ErrInvalid), "a squat replaying the member's proof is rejected")

	ok, err = witnessAnnouncePeerIdProof(after, meta("", ""), "no-peer")
	require.NoError(t, err, "an announce without a peer_id claims nothing")
	require.False(t, ok)
}
