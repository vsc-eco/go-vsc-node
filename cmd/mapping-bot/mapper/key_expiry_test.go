package mapper

import (
	"reflect"
	"strings"
	"testing"
)

// B8 (EVM bot D7 parity): warn for an expired key and for one expiring within
// keyExpiryWarnEpochs; nothing for a healthy, retired or expiry-less key.
func TestB8_KeyExpiryIssues(t *testing.T) {
	keys := map[string]tssKeyExpiry{
		"c-main":   {status: "active", expiry: 2473},      // 259 epochs left: fine
		"c-mainv1": {status: "active", expiry: 2214 + 28}, // at the warning edge
		"c-mainv2": {status: "deprecated", expiry: 2200},  // expired
		"c-mainv3": {status: "active", expiry: 0},         // no expiry: fine
		"c-mainv4": {status: "deleted", expiry: 100},      // retired: nothing to renew
	}
	got := keyExpiryIssues(keys, 2214)
	if len(got) != 2 {
		t.Fatalf("issues %v, want 2", got)
	}
	if !strings.Contains(got[0], "c-mainv1 expires in 28 epochs (~168h)") || !strings.Contains(got[1], "c-mainv2 has expired") {
		t.Fatalf("issues %v", got)
	}
	if got := keyExpiryIssues(map[string]tssKeyExpiry{"k": {status: "active", expiry: 2214 + 29}}, 2214); got != nil {
		t.Fatalf("29 epochs left warned: %v", got)
	}
	// past the expiry epoch but still "active" (status not flipped yet): warn with 0 left
	if got := keyExpiryIssues(map[string]tssKeyExpiry{"k": {status: "active", expiry: 2200}}, 2214); !reflect.DeepEqual(got, []string{"TSS key k expires in 0 epochs (~0h): run renewKey"}) {
		t.Fatalf("got %v", got)
	}
}
