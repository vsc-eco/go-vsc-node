package dids

import (
	"errors"
	"fmt"
	"slices"

	blocks "github.com/ipfs/go-block-format"
)

const VscDIDPrefix = "did:vsc"

var (
	_ DID = &VscDID{}

	ErrInsufficientThreshold = errors.New("not enough signatures")
	ErrInvalidMemberDID      = errors.New("invalid member DID")
	ErrInvalidBLSCircuit     = errors.New("invalid BLS circuit")
	ErrInvalidWeightMap      = errors.New("invalid member DID weight map")
	ErrInvalidVscDID         = errors.New("invalid VSC DID")
	ErrInvalidVSCThreshold   = errors.New("invalid VSC threshold")
)

// consensus DID for VSC internal
type VscDID struct {
	members   []BlsDID
	weightMap []uint64
	threshold uint64
	bitvector string
}

func NewVscDID(
	members []BlsDID,
	weightMap []uint64,
	bitVector string,
	threshold uint64,
) (*VscDID, error) {
	if len(members) != len(weightMap) {
		return nil, ErrInvalidWeightMap
	}

	vscDid := &VscDID{
		members:   members,
		weightMap: weightMap,
		threshold: threshold,
	}

	return vscDid, nil
}

// String implements DID.
func (v *VscDID) String() string {
	return fmt.Sprintf("%s:", VscDIDPrefix)
}

// Verify implements DID.
func (v *VscDID) Verify(data blocks.Block, sig string) (bool, error) {
	if v.threshold == 0 {
		return false, ErrInvalidVSCThreshold
	}
	// deserialize the BLS circuit
	var (
		circuit = SerializedCircuit{sig, v.bitvector}
		keyset  = v.members
		msg     = data.Cid()
	)

	blsCircuit, err := DeserializeBlsCircuit(circuit, keyset, msg)
	if err != nil {
		return false, fmt.Errorf("failed to deserialize BLS circuit: %w", err)
	}

	// verify the BLS circuit
	verified, includedDids, err := blsCircuit.Verify()
	if err != nil {
		return false, fmt.Errorf("failed to verify BLS circuit: %w", err)
	}

	if !verified {
		return false, ErrInvalidBLSCircuit
	}

	// calculate + verify signed weights
	signedWeight := uint64(0)
	for i := range includedDids {
		memberDidIndex := slices.Index(v.members, includedDids[i])
		if memberDidIndex == -1 {
			return false, ErrInvalidMemberDID
		}

		signedWeight += v.weightMap[memberDidIndex]
	}

	blockOk := signedWeight >= v.threshold
	if !blockOk {
		err = ErrInsufficientThreshold
	}

	return blockOk, err
}

//Things to do:
// - Transaction pool: verify incoming transactions
// - Parse DIDs from []string
