package dids

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/vsc-eco/hivego"
)

// Gateway-key proof-of-possession (ateway companion to the BLS
// VerifyBlsPoP). The gateway key is a Hive secp256k1 key (a key-auth in the
// vsc.gateway multisig), NOT a BLS key — so the consensus-key BLS PoP cannot
// vouch for it: it is a different curve/scheme, and the verifier (which holds
// only public keys) cannot re-derive one public key from the other because the
// link runs through the secret seed. Possession must therefore be proven by the
// gateway key signing its own account-bound message. Without this, the gateway
// key is an unauthenticated announced string: a distinct elected node could
// announce another member's public gateway key to force a duplicate-key
// account_update that Hive rejects (wedging gateway rotation).

// gatewayPoPDomain domain-separates the gateway-key PoP message from every other
// secp256k1 signature a node makes (Hive txs, gateway multisig votes), so a PoP
// can never be replayed as another signature, nor vice versa.
const gatewayPoPDomain = "vsc-gateway-key-pop:"

// gatewayPoPSigSize is the dcrd compact ECDSA signature layout (1 recovery byte
// + 32-byte R + 32-byte S) — the form secp256k1.SignCompact emits and
// RecoverCompact consumes.
const gatewayPoPSigSize = 1 + 32 + 32

// gatewayPoPHalfOrderN is N/2 of the secp256k1 group order (mirrors
// modules/gateway.IsLowS). A signature whose S exceeds it is the non-canonical
// high-S malleated twin; rejecting it keeps the PoP verdict robustly
// deterministic across nodes. Honest SignCompact output is always low-S.
var gatewayPoPHalfOrderN = new(big.Int).Rsh(secp256k1.S256().Params().N, 1)

// gatewayPoPDigest is the exact 32-byte message a witness signs with its gateway
// secp256k1 key to prove possession, bound to BOTH its Hive account and its
// announced gateway pubkey. Signer and verifier MUST build this identically.
// Binding the account stops a PoP being replayed under a different account;
// binding the key ties the proof to that exact announced gateway key.
func gatewayPoPDigest(gatewayKey string, account string) []byte {
	h := sha256.New()
	h.Write([]byte(gatewayPoPDomain))
	h.Write([]byte(gatewayKey))
	h.Write([]byte(account))
	return h.Sum(nil)
}

// GenerateGatewayKeyPoP returns a hex-encoded secp256k1 proof-of-possession for
// the node's gateway key, bound to account. An honest node — which holds the
// gateway private key derived from its BLS seed — can always produce it; a node
// that merely copied a victim's public gateway key cannot.
func GenerateGatewayKeyPoP(gatewayKP *hivego.KeyPair, account string) (string, error) {
	if gatewayKP == nil {
		return "", fmt.Errorf("gateway pop: nil key pair")
	}
	pubStr := gatewayKP.GetPublicKeyString()
	if pubStr == nil {
		return "", fmt.Errorf("gateway pop: nil public key")
	}
	sig, err := secp256k1.SignCompact(gatewayKP.PrivateKey, gatewayPoPDigest(*pubStr, account), true)
	if err != nil {
		return "", fmt.Errorf("gateway pop: sign: %w", err)
	}
	return hex.EncodeToString(sig), nil
}

// VerifyGatewayKeyPoP checks a hex-encoded secp256k1 proof-of-possession for
// gatewayKey, bound to account. Deterministic — a pure function of the announced
// gateway key, account, and PoP bytes — so every node reaches the same verdict
// and it is safe to gate election membership on. Returns an error when the
// key/PoP is missing, the signature is malformed or high-S, or it does not
// recover to the announced gateway key.
func VerifyGatewayKeyPoP(gatewayKey string, account string, pop string) error {
	if gatewayKey == "" {
		return fmt.Errorf("gateway pop: missing gateway key")
	}
	if pop == "" {
		return fmt.Errorf("gateway pop: missing proof-of-possession")
	}
	sigBytes, err := hex.DecodeString(pop)
	if err != nil {
		return fmt.Errorf("gateway pop: decode signature: %w", err)
	}
	if len(sigBytes) != gatewayPoPSigSize {
		return fmt.Errorf("gateway pop: invalid signature length: got %d, want %d", len(sigBytes), gatewayPoPSigSize)
	}
	// Reject the high-S malleated twin (S in the upper half of N) before
	// recovery, so a tampered-but-still-recoverable signature can't yield a
	// divergent verdict across nodes.
	s := new(big.Int).SetBytes(sigBytes[1+32:])
	if s.Sign() <= 0 || s.Cmp(gatewayPoPHalfOrderN) > 0 {
		return fmt.Errorf("gateway pop: non-canonical high-S signature")
	}
	pubKey, _, err := secp256k1.RecoverCompact(sigBytes, gatewayPoPDigest(gatewayKey, account))
	if err != nil {
		return fmt.Errorf("gateway pop: recover: %w", err)
	}
	recovered := hivego.GetPublicKeyString(pubKey)
	if recovered == nil || *recovered != gatewayKey {
		return fmt.Errorf("gateway pop: recovered key does not match announced gateway key")
	}
	return nil
}
