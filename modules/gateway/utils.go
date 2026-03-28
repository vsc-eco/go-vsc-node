package gateway

import (
	"crypto/sha256"
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

func GatewayKeyFromBlsSeed(blsSeed string) (*hivego.KeyPair, error) {
	blsPrivSeed, err := hex.DecodeString(blsSeed)
	if err != nil {
		return nil, err
	}
	salt := []byte("gateway_key")
	gatewayKey := sha256.Sum256(append(blsPrivSeed, salt...))

	kp := hivego.KeyPairFromBytes(gatewayKey[:])
	return kp, nil
}
