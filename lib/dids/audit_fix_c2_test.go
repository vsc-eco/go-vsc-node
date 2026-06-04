package dids

import (
	"testing"

	"github.com/multiformats/go-multibase"
)

// TestFixGVL4_ParseBlsDIDShortBody — regression for GV-L4. Before the fix,
// ParseBlsDID panicked on a <48-byte body via the (*[48]byte) cast. After the
// fix it must return an error (no panic).
func TestFixGVL4_ParseBlsDIDShortBody(t *testing.T) {
	// VALID BLS multicodec prefix (0xea,0x01) + a 3-byte short body, so we pass
	// the prefix guard at bls.go:82 and actually reach the new length check
	// (data[2:] = 3 bytes != 48). A zero-prefix body would be caught earlier and
	// the test would pass even without the fix (scrutiny C2 catch).
	data := append([]byte{0xea, 0x01}, make([]byte, 3)...)
	enc, err := multibase.Encode(multibase.Base58BTC, data)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	did := BlsDIDPrefix + enc
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GV-L4 regressed: ParseBlsDID panicked instead of erroring: %v", r)
		}
	}()
	if _, err := ParseBlsDID(did); err == nil {
		t.Fatal("GV-L4: short BLS pub key body must return an error, got nil")
	}
}

// TestFixGVL5_FloatAmount — regression for GV-L5. Non-integer / out-of-range
// floats must now error instead of being silently truncated/wrapped; valid
// integer amounts still convert.
func TestFixGVL5_FloatAmount(t *testing.T) {
	if _, err := EIP712AmountFloat(1.5); err == nil {
		t.Fatal("GV-L5: non-integer float 1.5 must error")
	}
	if _, err := EIP712AmountFloat(9.3e18); err == nil {
		t.Fatal("GV-L5: out-of-int64-range float must error")
	}
	if v, err := EIP712AmountFloat(100); err != nil || v.Int64() != 100 {
		t.Fatalf("GV-L5: valid integer 100 must convert, got v=%v err=%v", v, err)
	}
}
