package dids

import (
	"errors"
	"fmt"

	blocks "github.com/ipfs/go-block-format"
)

// ===== DIDs =====

type DID interface {
	String() string
	Verify(data blocks.Block, sig string) (bool, error)
}

// ===== provider interface (can be passed around later, depending on how DIDs want to be used) =====

type Provider[V any] interface {
	Sign(data V) (string, error)
}

func Parse(did string, includeBLS ...bool) (DID, error) {
	errs := []error{}

	if len(includeBLS) > 0 && includeBLS[0] {
		res, err := ParseBlsDID(did)
		if err == nil {
			return res, nil
		}
		errs = append(errs, err)
	}

	var res DID

	res, err := ParseKeyDID(did)
	if err == nil {
		return res, nil
	}
	errs = append(errs, err)

	res, err = ParseEthDID(did)
	if err == nil {
		return res, nil
	}
	errs = append(errs, err)

	return nil, errors.Join(errs...)
}

func ParseMany(dids []string, includeBLS ...bool) ([]DID, error) {
	res := make([]DID, len(dids))
	var err error

	for i, did := range dids {
		res[i], err = Parse(did, includeBLS...)
		if err != nil {
			return nil, fmt.Errorf("could not parse did %d: %w", i, err)
		}
	}

	return res, nil
}

func VerifyMany(dids []DID, blk blocks.Block, sigs []string) (bool, []bool, error) {
	if len(dids) != len(sigs) {
		return false, nil, fmt.Errorf("len(dids) != len(sigs)")
	}

	res := true
	results := make([]bool, len(dids))
	for i, did := range dids {
		sig := sigs[i]
		verified, err := did.Verify(blk, sig)
		if err != nil {
			return false, nil, err
		}

		if !verified {
			res = false
		}

		results[i] = verified
	}

	return res, results, nil
}
