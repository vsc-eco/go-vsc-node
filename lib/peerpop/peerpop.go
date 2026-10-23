// Package peerpop is the proof that a witness announcement's peer_id belongs to
// the announcer (B1, testnet 2026-10-22).
//
// TSS attributes a round message to the account whose announcement carries the
// sending libp2p peer id. That peer id is a free-form string in the account's own
// account_update metadata, and unlike the consensus BLS key and the gateway key it
// carried no proof of possession, so any account could announce another member's
// peer id and make the member's messages resolve to the wrong account (dropped by
// the dispatcher). The proof is a signature by the libp2p key over the account and
// the peer id; peer ids are ed25519 identity hashes, so the key is recovered from the
// peer id itself and nothing else needs to be announced.
package peerpop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

const domain = "vsc-peerid-pop-v1"

var (
	// ErrMissing: the announcement carries a peer id but no proof.
	ErrMissing = errors.New("peer id proof of possession missing")
	// ErrInvalid: the proof does not verify for this account and peer id.
	ErrInvalid = errors.New("peer id proof of possession invalid")
)

// Message is the signed digest, bound to both the account and the peer id so a
// proof cannot be replayed under another account or for another peer id.
func Message(account, peerId string) []byte {
	h := sha256.Sum256([]byte(domain + "|" + account + "|" + peerId))
	return h[:]
}

// Sign proves possession of priv for peerId, which must be priv's own peer id.
func Sign(priv crypto.PrivKey, account, peerId string) (string, error) {
	own, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return "", err
	}
	if own.String() != peerId {
		return "", fmt.Errorf("peer id %s is not this node's key (%s)", peerId, own)
	}
	sig, err := priv.Sign(Message(account, peerId))
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig), nil
}

// Verify checks popHex for account and peerId. Pure and deterministic: every node
// reaches the same verdict from the announcement alone.
func Verify(account, peerId, popHex string) error {
	if popHex == "" {
		return ErrMissing
	}
	pid, err := peer.Decode(peerId)
	if err != nil {
		return fmt.Errorf("%w: undecodable peer id: %v", ErrInvalid, err)
	}
	pub, err := pid.ExtractPublicKey()
	if err != nil {
		return fmt.Errorf("%w: peer id does not embed its key: %v", ErrInvalid, err)
	}
	sig, err := hex.DecodeString(popHex)
	if err != nil {
		return fmt.Errorf("%w: undecodable proof: %v", ErrInvalid, err)
	}
	ok, err := pub.Verify(Message(account, peerId), sig)
	if err != nil || !ok {
		return ErrInvalid
	}
	return nil
}
