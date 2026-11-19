package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
)

// TestH3_RecoverPublicKey_RejectsZeroR is the regression guard for H3
// (RV re-verify): a compact signature with R == 0 and a valid low-S must be
// REJECTED with an error, not crash the process. Pre-fix it reached
// secp256k1/v2 v2.0.1 RecoverCompact, which computes ModInverse(0, N) == nil
// and then nil-panics in big.Int.Mul — and because `sign_response` is accepted
// from any peer and recovered inside the un-recovered waitForSigs collector
// goroutine, that panic took down the whole signing node.
func TestH3_RecoverPublicKey_RejectsZeroR(t *testing.T) {
	hash := sha256.Sum256([]byte("h3"))

	lowS := make([]byte, 32)
	big.NewInt(1).FillBytes(lowS) // S = 1: low-S and > 0, so it passes IsLowS

	for _, hdr := range []byte{27, 28, 29, 30} {
		sig := make([]byte, 0, 65)
		sig = append(sig, hdr)
		sig = append(sig, make([]byte, 32)...) // R = 0
		sig = append(sig, lowS...)

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("hdr=%d: RecoverPublicKey PANICKED on R=0 (H3 regression): %v", hdr, r)
				}
			}()
			pk, err := RecoverPublicKey(hex.EncodeToString(sig), hash[:])
			if err == nil {
				t.Fatalf("hdr=%d: expected an error for R=0, got pk=%q", hdr, pk)
			}
		}()
	}
}

// TestH3_RecoverPublicKey_AcceptsGenuine confirms the guard does not reject a
// real signature: a genuine low-S compact signature still recovers its signer.
func TestH3_RecoverPublicKey_AcceptsGenuine(t *testing.T) {
	hash := sha256.Sum256([]byte("genuine"))
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	csig, err := secp256k1.SignCompact(priv, hash[:], false)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := RecoverPublicKey(hex.EncodeToString(csig), hash[:])
	if err != nil {
		t.Fatalf("genuine signature must recover, got err: %v", err)
	}
	if pk == "" {
		t.Fatal("genuine signature recovered an empty pubkey")
	}
}
