package tss_helpers

import (
	"crypto/elliptic"
	"errors"
	"math"
	"math/big"

	"github.com/bnb-chain/tss-lib/v2/tss"
	"github.com/eager7/dogd/btcec"
	"github.com/libp2p/go-libp2p/core/peer"
)

func MsgToHashInt(msg []byte, algo SigningAlgo) (*big.Int, error) {
	switch algo {
	case SigningAlgoEcdsa:
		return hashToInt(msg, btcec.S256()), nil
	case SigningAlgoEddsa:
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
	threshold := int(math.Ceil(float64(value)*2.0/3.0)) - 1
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
