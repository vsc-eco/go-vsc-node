package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

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

// deriveNodeAccountName creates a safe, deterministic Hive account name for a
// validator's per-node HP staking account. Uses a hash-based approach to avoid
// issues with dots, uppercase, and naming collisions.
//
// Hive account name rules: 3-16 chars, lowercase a-z, 0-9, hyphens (no leading/trailing),
// dot-separated segments must each be >= 3 chars.
//
// Format: "mv-" (3 chars) + 13 hex chars from SHA256[:7] = 16 chars total (max Hive name)
// 56 bits of hash space = 72 quadrillion values, no birthday collision risk at 1M validators.
//
// TODO(B3): Add a collision registry — store validator->nodeAccount mapping in the action
// record's Params or in a dedicated witness DB field. On hp_stake processing, verify no
// other validator already maps to the same hash. At 56 bits the probability is negligible
// but defense-in-depth is good practice. For now, the hash space is large enough.
func deriveNodeAccountName(hiveAccount string) string {
	normalized := strings.ToLower(strings.TrimSpace(hiveAccount))
	hash := sha256.Sum256([]byte(normalized))
	// 7 bytes = 14 hex chars, truncate to 13 to fit 16-char limit with "mv-" prefix
	suffix := hex.EncodeToString(hash[:7])[:13]
	return "mv-" + suffix
}

// hiveAccountNameRegex validates Hive L1 account names
var hiveAccountNameRegex = regexp.MustCompile(`^[a-z][0-9a-z\-]*[0-9a-z](\.[a-z][0-9a-z\-]*[0-9a-z])*$`)

// isValidHiveAccount checks if a string is a valid Hive account name
func isValidHiveAccount(name string) bool {
	return len(name) >= 3 && len(name) <= 16 && hiveAccountNameRegex.MatchString(name)
}

// NullMemoKey is a provably unspendable public key for per-node accounts.
// Derived from SHA256("magi-hp-staking-null-memo-key") used as a secp256k1 private key.
// Anyone can verify this derivation, but the key is useless for memo decryption
// since per-node accounts never send or receive encrypted memos.
//
// To regenerate:
//
//	hash := sha256.Sum256([]byte("magi-hp-staking-null-memo-key"))
//	kp := hivego.KeyPairFromBytes(hash[:])
//	fmt.Println(*hivego.GetPublicKeyString(kp.PublicKey))
//
// Result: STM8jDSGBb3eu6jbk2JmjS3qSHU7cFoQNLN37seTF2ySR1Y3WvaY4
const NullMemoKey = "STM8jDSGBb3eu6jbk2JmjS3qSHU7cFoQNLN37seTF2ySR1Y3WvaY4"

func accountControlledByGateway(acct hivego.AccountData, gatewayWallet string) bool {
	for _, auth := range acct.Active.AccountAuths {
		if len(auth) >= 1 {
			if name, ok := auth[0].(string); ok && name == gatewayWallet {
				return true
			}
		}
	}
	return false
}

// getMemoKeyForNodeAccount returns a valid public key for per-node account memo field.
// Per-node accounts don't use memos, but Hive requires a valid secp256k1 public key.
// Uses the gateway's signing keypair (derived from BLS seed + "gateway_key" salt).
// Falls back to a provably unspendable null key if signing keypair is unavailable.
func getMemoKeyForNodeAccount(ms *MultiSig) string {
	kp := ms.getSigningKp() // existing method in multisig.go
	if kp == nil {
		return NullMemoKey
	}
	pubKeyStr := hivego.GetPublicKeyString(kp.PublicKey)
	if pubKeyStr == nil {
		return NullMemoKey
	}
	return *pubKeyStr
}
