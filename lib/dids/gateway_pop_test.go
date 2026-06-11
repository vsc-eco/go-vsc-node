package dids

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/vsc-eco/hivego"
)

func gwKP(t *testing.T, b byte) *hivego.KeyPair {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = b
	}
	return hivego.KeyPairFromBytes(seed)
}

func TestGatewayKeyPoP_RoundTrip(t *testing.T) {
	kp := gwKP(t, 0x11)
	key := *kp.GetPublicKeyString()

	pop, err := GenerateGatewayKeyPoP(kp, "alice")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := VerifyGatewayKeyPoP(key, "alice", pop); err != nil {
		t.Fatalf("valid PoP rejected: %v", err)
	}
}

func TestGatewayKeyPoP_Rejections(t *testing.T) {
	kp := gwKP(t, 0x11)
	key := *kp.GetPublicKeyString()
	pop, err := GenerateGatewayKeyPoP(kp, "alice")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Wrong account: PoP is account-bound, so it must not verify under another.
	if err := VerifyGatewayKeyPoP(key, "bob", pop); err == nil {
		t.Fatal("PoP verified under the wrong account")
	}

	// Wrong announced key: recovered pubkey won't match a different key string.
	otherKey := *gwKP(t, 0x22).GetPublicKeyString()
	if err := VerifyGatewayKeyPoP(otherKey, "alice", pop); err == nil {
		t.Fatal("PoP verified against a different announced key")
	}

	// Missing inputs.
	if err := VerifyGatewayKeyPoP("", "alice", pop); err == nil {
		t.Fatal("empty key accepted")
	}
	if err := VerifyGatewayKeyPoP(key, "alice", ""); err == nil {
		t.Fatal("empty PoP accepted")
	}

	// Tampered signature (flip a byte in R).
	raw, _ := hex.DecodeString(pop)
	raw[5] ^= 0xff
	if err := VerifyGatewayKeyPoP(key, "alice", hex.EncodeToString(raw)); err == nil {
		t.Fatal("tampered PoP accepted")
	}
}

// TestGatewayKeyPoP_ForgeryBlocked is the security property: a distinct node
// (the attacker, with its own key) cannot announce the VICTIM's public gateway
// key with a valid PoP — its best attempt signs the victim-key digest with its
// own private key, which recovers to the attacker's key, not the victim's. This
// is exactly the duplicate-key griefing the PoP closes.
func TestGatewayKeyPoP_ForgeryBlocked(t *testing.T) {
	victimKey := *gwKP(t, 0x11).GetPublicKeyString()
	attacker := gwKP(t, 0x22)

	// Attacker signs the victim-key/attacker-account digest with its OWN key.
	forged, err := secp256k1.SignCompact(attacker.PrivateKey, gatewayPoPDigest(victimKey, "attacker"), true)
	if err != nil {
		t.Fatalf("forge sign: %v", err)
	}
	if err := VerifyGatewayKeyPoP(victimKey, "attacker", hex.EncodeToString(forged)); err == nil {
		t.Fatal("attacker forged a valid PoP for the victim's gateway key")
	} else if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected key-mismatch rejection, got: %v", err)
	}
}
