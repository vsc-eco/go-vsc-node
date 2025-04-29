package gateway

import (
	"encoding/hex"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/vsc-eco/hivego"
)

func RecoverPublicKey(signature string, hash []byte) (string, error) {
	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return "", err
	}
	pubKey, _, err := secp256k1.RecoverCompact(sigBytes, hash)

	if err != nil {
		return "", err
	}
	return *hivego.GetPublicKeyString(pubKey), nil
}
