package btcvault

import (
	"bytes"
	"testing"
)

func TestCheckSigMessage_Deterministic32(t *testing.T) {
	pk := make([]byte, 33)
	pk[0] = 2
	pk[32] = 7
	a := CheckSigMessage("vsc1abc-mainv3", 3, pk)
	b := CheckSigMessage("vsc1abc-mainv3", 3, pk)
	if !bytes.Equal(a, b) {
		t.Fatal("CheckSigMessage not deterministic for identical inputs")
	}
	if len(a) != 32 {
		t.Fatalf("want a 32-byte digest, got %d", len(a))
	}
}

// TestCheckSigMessage_BindsEveryField — M must change if keyId, gen, or pubkey
// changes, so a valid check-sig can never be replayed onto a different key or
// generation (and a real withdrawal sig, over a different message, never
// satisfies it).
func TestCheckSigMessage_BindsEveryField(t *testing.T) {
	pk := make([]byte, 33)
	pk[0] = 2
	pk2 := make([]byte, 33)
	pk2[0] = 3
	base := CheckSigMessage("vsc1abc-mainv3", 3, pk)
	if bytes.Equal(base, CheckSigMessage("vsc1abc-mainv4", 3, pk)) {
		t.Fatal("keyId not bound into M")
	}
	if bytes.Equal(base, CheckSigMessage("vsc1abc-mainv3", 4, pk)) {
		t.Fatal("generation not bound into M")
	}
	if bytes.Equal(base, CheckSigMessage("vsc1abc-mainv3", 3, pk2)) {
		t.Fatal("pubkey not bound into M")
	}
}

// TestCheckSigMessage_LengthDelimitedNoAmbiguity — the uint16 keyId length
// prefix means two different (keyId, gen) splits cannot alias to the same
// preimage even if their naive concatenation would coincide.
func TestCheckSigMessage_LengthDelimitedNoAmbiguity(t *testing.T) {
	pk := make([]byte, 33)
	// "ab" + gen bytes ... vs "a" + "b"-shifted: with a length prefix these
	// differ. Use keyIds whose concatenation with the fixed-width gen would
	// otherwise be confusable.
	if bytes.Equal(CheckSigMessage("aab", 1, pk), CheckSigMessage("aa", 0x62 /* 'b' */, pk)) {
		t.Fatal("length prefix failed to disambiguate keyId/gen boundary")
	}
}

func TestVaultGenFromKeyId(t *testing.T) {
	const c = "vsc1contract"
	cases := []struct {
		keyId string
		gen   uint32
		ok    bool
	}{
		{c + "-main", 0, true},
		{c + "-mainv1", 1, true},
		{c + "-mainv42", 42, true},
		{c + "-mainv4294967295", 4294967295, true},
		{c + "-mainv0", 0, false},  // non-canonical: gen 0 is "main"
		{c + "-mainv01", 0, false}, // leading zero
		{c + "-mainv", 0, false},   // empty number
		{c + "-main5", 0, false},   // not the "mainv" shape
		{c + "-mainvx", 0, false},  // non-numeric
		{c + "-other", 0, false},   // not a vault key name
		{"wrongprefix-main", 0, false},
		{c + "-mainv99999999999999999999", 0, false}, // overflows uint32
	}
	for _, tc := range cases {
		gen, ok := VaultGenFromKeyId(c, tc.keyId)
		if ok != tc.ok || (ok && gen != tc.gen) {
			t.Fatalf("VaultGenFromKeyId(%q, %q) = %d,%v; want %d,%v", c, tc.keyId, gen, ok, tc.gen, tc.ok)
		}
	}
	// Empty contract id is never a vault key.
	if _, ok := VaultGenFromKeyId("", "-main"); ok {
		t.Fatal("empty contract id must not resolve")
	}
}

// TestVaultGenRoundTrip — VaultGenFromKeyId is the exact inverse of VaultKeyName
// across the full uint32 range boundaries.
func TestVaultGenRoundTrip(t *testing.T) {
	const c = "vsc1contract"
	for _, g := range []uint32{0, 1, 2, 9, 10, 100, 65535, 4294967295} {
		keyId := c + "-" + VaultKeyName(g)
		gen, ok := VaultGenFromKeyId(c, keyId)
		if !ok || gen != g {
			t.Fatalf("round trip gen %d: keyId=%q -> %d,%v", g, keyId, gen, ok)
		}
	}
}
