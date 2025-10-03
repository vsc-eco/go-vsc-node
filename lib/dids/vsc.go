package dids

import (
	"errors"
	"fmt"
	"slices"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
)

type VscDIDType string

const (
	vscDIDPrefix = "did:vsc"
)

var (
	_ DID = &VscDID{}

	ErrInsufficientThreshold = errors.New("not enough signatures")
	ErrInvalidMemberDID      = errors.New("invalid member DID")
	ErrInvalidBLSCircuit     = errors.New("invalid BLS circuit")
	ErrInvalidWeightMap      = errors.New("invalid member DID weight map")
)

// consensus DID for VSC internal
type VscDID struct {
	didType         string
	memberDids      []BlsDID
	memberDidWeight []int
	signature       SerializedCircuit
	hash            cid.Cid
	threshold       int
}

func NewVscDID(
	vscDidType string,
	memberDids []BlsDID,
	memberDidWeights []int,
	hash cid.Cid,
	signature SerializedCircuit,
	threshold int,
) (*VscDID, error) {
	if len(memberDids) != len(memberDidWeights) {
		return nil, ErrInvalidWeightMap
	}

	vscDid := &VscDID{
		didType:         vscDidType,
		memberDids:      memberDids,
		memberDidWeight: memberDidWeights,
		signature:       signature,
		hash:            hash,
		threshold:       threshold,
	}

	return vscDid, nil
}

// String implements DID.
func (v *VscDID) String() string {
	return fmt.Sprintf("%s:%s", vscDIDPrefix, v.didType)
}

// Verify implements DID.
func (v *VscDID) Verify(data blocks.Block, sig string) (bool, error) {
	// deserialize + verify the BLS circuit
	blsCircuit, err := DeserializeBlsCircuit(v.signature, v.memberDids, v.hash)
	if err != nil {
		return false, fmt.Errorf("failed to deserialize BLS circuit: %w", err)
	}

	verified, includedDids, err := blsCircuit.Verify()
	if err != nil {
		return false, fmt.Errorf("failed to verify BLS circuit: %w", err)
	}

	if !verified {
		return false, ErrInvalidBLSCircuit
	}

	// calculate + verify signed weights
	signedWeight := 0
	for i := range includedDids {
		memberDidIndex := slices.Index(v.memberDids, includedDids[i])
		if memberDidIndex == -1 {
			return false, ErrInvalidMemberDID
		}

		signedWeight += v.memberDidWeight[memberDidIndex]
	}

	blockOk := signedWeight >= v.threshold
	if !blockOk {
		err = ErrInsufficientThreshold
	}

	return blockOk, err
}
