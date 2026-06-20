package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/vsc-eco/hivego"
)

// secpCompactSigSize is the byte layout of a dcrd compact ECDSA signature
// (1 header byte + 32-byte R + 32-byte S). Used by IsLowS to reach the S
// component without re-parsing the whole signature.
const secpCompactSigSize = 1 + 32 + 32

// halfOrderN is N/2 of the secp256k1 group order, precomputed once. Any
// signature with S > halfOrderN is non-canonical (high-S form) and is the
// trivial malleated counterpart of a valid low-S signature.
var halfOrderN = new(big.Int).Rsh(secp256k1.S256().Params().N, 1)

// IsLowS reports whether the S component of a compact ECDSA signature is
// in the lower half of the group order. BIP-62 / RFC 6979 low-S form is
// the canonical form; rejecting high-S kills the (r, N-s) malleability
// twin that S1 exploits to double-count a single signer's gateway vote.
func IsLowS(sigBytes []byte) bool {
	if len(sigBytes) != secpCompactSigSize {
		return false
	}
	s := new(big.Int).SetBytes(sigBytes[1+32:])
	return s.Sign() > 0 && s.Cmp(halfOrderN) <= 0
}

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
	// Length is checked here so the high-S branch below can't conflate
	// "wrong byte count" with "non-canonical S" in operator logs.
	if len(sigBytes) != secpCompactSigSize {
		return "", fmt.Errorf("invalid compact signature length: got %d, want %d", len(sigBytes), secpCompactSigSize)
	}
	// S1: reject the high-S malleated twin of any valid signature before
	// pubkey recovery. Without this, (r, N-s) recovers the same pubkey but
	// presents a different sig string, defeating the dedup at collectSigs.
	if !IsLowS(sigBytes) {
		return "", errors.New("signature has non-canonical high-S value")
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
	// MED M26-K1-M2 (#54): scrub the transient secret material from the heap
	// before returning. getSigningKp re-derives on every rotation tick, so these
	// buffers (the raw BLS private seed and the derived gateway private key)
	// otherwise pile up on the heap and can survive into a core dump / swap page /
	// heap snapshot taken during a renew/retire window. This is the gateway-side
	// companion to the TSS-module zeroize and is pure defense-in-depth: it never
	// changes the returned key bytes.
	//
	// Capturing the appended buffer is required because append() may allocate a
	// NEW backing array (cap exceeded) that ALSO holds a copy of blsPrivSeed;
	// wiping only blsPrivSeed would leave that second copy live. KeyPairFromBytes
	// copies the input into a fresh big.Int (secp256k1.PrivKeyFromBytes →
	// big.Int.SetBytes), so the returned KeyPair does not alias gatewayKey — the
	// wipe below is byte-safe and behaviour-neutral.
	seedAndSalt := append(blsPrivSeed, []byte("gateway_key")...)
	gatewayKey := sha256.Sum256(seedAndSalt)
	defer func() {
		for i := range blsPrivSeed {
			blsPrivSeed[i] = 0
		}
		for i := range seedAndSalt {
			seedAndSalt[i] = 0
		}
		for i := range gatewayKey {
			gatewayKey[i] = 0
		}
	}()

	kp := hivego.KeyPairFromBytes(gatewayKey[:])
	return kp, nil
}
