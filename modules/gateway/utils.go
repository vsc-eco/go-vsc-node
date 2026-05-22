package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/vsc-eco/hivego"
)

// hivePublicKeyPrefix is the Hive base58 public-key prefix. Kept in sync with
// hivego.PublicKeyPrefix; copied locally so the FUZZ-1 guard can length-check
// without round-tripping through the panicking decoder.
const hivePublicKeyPrefix = "STM"

// hivePublicKeyMinLen is a conservative lower bound on a well-formed Hive
// public key string. A genuine 33-byte compressed pubkey + 4-byte checksum
// base58-encodes to ~50 chars; we accept anything ≥ prefix+30 as "shape ok"
// and rely on hivego.DecodePublicKey (called inside the recover wrapper) to
// reject the remainder.
const hivePublicKeyMinLen = len(hivePublicKeyPrefix) + 30

// safeValidateGatewayKey wraps hivego.DecodePublicKey in a recover so that
// adversary-controlled witness GatewayKey strings (FUZZ-1) cannot crash the
// node when they are about to be serialized into a Hive multisig-rotation tx.
//
// Why: hivego.DecodePublicKey (keys.go:41) slices decoded[len-4:] without a
// length check. Inputs like "STM" or any base58 body that decodes to <4 bytes
// trigger "slice bounds out of range [-4:]" panic. That panic fires inside
// the unrecovered TickKeyRotation goroutine and takes down vsc-node entirely
// for every elected witness on the same rotation block. The upstream fix
// belongs in vsc-eco/hivego; this guard makes the node defensive even before
// that lands.
func safeValidateGatewayKey(key string) (err error) {
	if len(key) < hivePublicKeyMinLen {
		return fmt.Errorf("gateway key too short (%d chars)", len(key))
	}
	if key[:len(hivePublicKeyPrefix)] != hivePublicKeyPrefix {
		return errors.New("gateway key missing STM prefix")
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("gateway key decoder panic: %v", r)
		}
	}()
	if _, decodeErr := hivego.DecodePublicKey(key); decodeErr != nil {
		return decodeErr
	}
	return nil
}

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
