package transactionpool

import (
	"errors"
	"vsc-node/lib/dids"

	"github.com/ipfs/go-cid"
)

func VerifyTxSignatures(cidz cid.Cid, sigs SignaturePackage) (bool, []string, error) {

	verifiedDids := make([]string, 0)
	for _, v := range sigs.Sigs {

		if v.Alg != "EdDSA" {
			return false, nil, errors.New("Invalid algorithm")
		}

		if v.Kid == "" {
			return false, nil, errors.New("No key id provided")
		}

		if v.Sig == "" {
			return false, nil, errors.New("No signature provided")
		}

		keyDid := dids.KeyDID(v.Kid)

		verified, err := keyDid.Verify(cidz, v.Sig)

		if !verified {
			return false, verifiedDids, err
		}
		verifiedDids = append(verifiedDids, keyDid.String())
	}
	return true, verifiedDids, nil
}
