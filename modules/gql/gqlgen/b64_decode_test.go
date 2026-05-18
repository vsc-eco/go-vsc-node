package gqlgen

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestDecodeSubmittedB64(t *testing.T) {
	// Bytes chosen so StdEncoding produces '+' and '/' (the exact failure
	// the bug report reproduced on testnet: "AAEC+wQF/gcI").
	raw := []byte{0x00, 0x01, 0x02, 0xfb, 0x04, 0x05, 0xfe, 0x07, 0x08}

	std := base64.StdEncoding.EncodeToString(raw)
	if !strings.ContainsAny(std, "+/") {
		t.Fatalf("test fixture invalid: std encoding %q has no + or /", std)
	}

	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"std with + and /", std, true},
		{"url-safe", base64.URLEncoding.EncodeToString(raw), true},
		{"raw std unpadded", base64.RawStdEncoding.EncodeToString(raw), true},
		{"raw url unpadded", base64.RawURLEncoding.EncodeToString(raw), true},
		{"garbage", "!!!not base64!!!", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeSubmittedB64(c.in)
			if c.ok {
				if err != nil {
					t.Fatalf("expected ok, got err: %v", err)
				}
				if string(got) != string(raw) {
					t.Fatalf("bytes mismatch: got %x want %x", got, raw)
				}
			} else if err == nil {
				t.Fatalf("expected error for %q", c.in)
			}
		})
	}
}
