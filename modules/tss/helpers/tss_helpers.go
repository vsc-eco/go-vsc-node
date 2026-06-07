package tss_helpers

import (
	"crypto/elliptic"
	"errors"
	"fmt"
	"math/big"

	"github.com/bnb-chain/tss-lib/v3/tss"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/libp2p/go-libp2p/core/peer"
)

// secp256k1DigestBytes is the curve-order byte length for secp256k1 — the size
// of a well-formed ECDSA message digest (a BTC/ETH sighash is exactly this).
const secp256k1DigestBytes = 32

// MsgToHashInt converts a sign-request message into the scalar the threshold
// signing party signs.
//
// GV-H4: the ECDSA path must reject a non-32-byte digest rather than silently
// truncating it. hashToInt drops every byte past the 32nd, so two different
// messages sharing a 32-byte prefix would be signed to the SAME scalar — one
// threshold signature valid for both. We deliberately do NOT re-hash or
// domain-tag here: the resulting ECDSA signature must verify on the destination
// chain (BTC/ETH) over the exact 32-byte sighash, and any transformation would
// make it unverifiable there. Cross-key replay across purposes is separately
// prevented by the per-contract key namespace (contractId-keyId) at the call
// site. A valid 32-byte digest is therefore signed verbatim — unchanged.
func MsgToHashInt(msg []byte, algo SigningAlgo) (*big.Int, error) {
	switch algo {
	case SigningAlgoEcdsa:
		if len(msg) != secp256k1DigestBytes {
			return nil, fmt.Errorf("ecdsa sign message must be a %d-byte digest, got %d bytes", secp256k1DigestBytes, len(msg))
		}
		return hashToInt(msg, btcec.S256()), nil
	case SigningAlgoEddsa:
		if len(msg) == 0 {
			return nil, errors.New("eddsa sign message must not be empty")
		}
		// directly convert the byte array to big int
		return new(big.Int).SetBytes(msg), nil
	default:
		return nil, errors.New("invalid algo")
	}
}

func hashToInt(hash []byte, c elliptic.Curve) *big.Int {
	orderBits := c.Params().BitSize
	orderBytes := (orderBits + 7) / 8
	if len(hash) > orderBytes {
		hash = hash[:orderBytes]
	}

	ret := new(big.Int).SetBytes(hash)
	excess := len(hash)*8 - orderBits
	if excess > 0 {
		ret.Rsh(ret, uint(excess))
	}
	return ret
}

func GetThreshold(value int) (int, error) {
	if value < 0 {
		return 0, errors.New("negative input")
	}
	// Integer-only ceil (audit M-9): the threshold is ceil(2*value/3) - 1, the
	// btss Lagrange/Shamir polynomial degree that EVERY node must agree on
	// (it feeds NewReSharingParameters + the pre-flight quorum gates). float64
	// ceil in a consensus path violates the determinism rule, so compute it in
	// pure integers: ceil(a/b) = (a + b - 1) / b, here a=2*value, b=3 ⇒
	// (2*value + 2) / 3. Identical to the old float form for all value >= 0
	// (IEEE 3k/3 is exact; non-integer cases sit well away from a ceil
	// boundary), with zero float dependence. Verified: 19→12, 15→9, 13→8.
	threshold := (2*value+2)/3 - 1
	return threshold, nil
}

// func GetParties(keys []string, localPartyKey string) (btss.SortedPartyIDs, *btss.PartyID, error) {
// 	var localPartyID *btss.PartyID
// 	var unSortedPartiesID []*btss.PartyID
// 	sort.Strings(keys)
// 	for idx, item := range keys {

// 		pk, err := sdk.UnmarshalPubKey(sdk.AccPK, item) // nolint:staticcheck
// 		if err != nil {
// 			return nil, nil, fmt.Errorf("fail to get account pub key address(%s): %w", item, err)
// 		}
// 		key := new(big.Int).SetBytes(pk.Bytes())
// 		// Set up the parameters
// 		// Note: The `id` and `moniker` fields are for convenience to allow you to easily track participants.
// 		// The `id` should be a unique string representing this party in the network and `moniker` can be anything (even left blank).
// 		// The `uniqueKey` is a unique identifying key for this peer (such as its p2p public key) as a big.Int.
// 		partyID := btss.NewPartyID(strconv.Itoa(idx), "", key)
// 		if item == localPartyKey {
// 			localPartyID = partyID
// 		}
// 		unSortedPartiesID = append(unSortedPartiesID, partyID)
// 	}
// 	if localPartyID == nil {
// 		return nil, nil, errors.New("local party is not in the list")
// 	}

// 	partiesID := btss.SortPartyIDs(unSortedPartiesID)
// 	return partiesID, localPartyID, nil
// }

type PartyMember struct {
	PeerId peer.ID
	//Hive account name of the party's witness node
	Account string
}

func (pm *PartyMember) PartyId() *tss.PartyID {
	bytes, _ := pm.PeerId.MarshalBinary()

	return tss.NewPartyID("", "", new(big.Int).SetBytes(bytes))
}

// func SelectCommitment(keyId string, blockHeight uint64, commitments tss_db.TssCommitments) (tss_db.TssCommitment, error) {

// 	commitments.GetCommitment()
// }
