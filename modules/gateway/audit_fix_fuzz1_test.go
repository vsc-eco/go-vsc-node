package gateway

// Post-fix companion to modules/gateway/audit_unfixed_test.go (FUZZ-1).
//
// The differential test in audit_unfixed_test.go still asserts the *upstream*
// hivego.DecodePublicKey panic, because the upstream fix in vsc-eco/hivego is
// tracked separately. This file asserts the in-tree guard so the gateway
// rotation path can no longer be crashed by a poisoned witness gateway_key.

import (
	"strings"
	"testing"
)

// TestAuditFix_FUZZ1_SafeValidateGatewayKey_NoPanicOnShortInput drives the
// exact input that crashes hivego.DecodePublicKey ("STM") and asserts the
// in-tree wrapper returns an error instead of panicking.
func TestAuditFix_FUZZ1_SafeValidateGatewayKey_NoPanicOnShortInput(t *testing.T) {
	poisons := []string{
		"STM",
		"STM1",
		"STM11",
		"STM111",
		"",
		"hello",
		strings.Repeat("x", 10),
		"STM" + strings.Repeat("z", 10),
	}
	for _, in := range poisons {
		in := in
		t.Run(in, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("safeValidateGatewayKey(%q) panicked: %v", in, r)
				}
			}()
			if err := safeValidateGatewayKey(in); err == nil {
				t.Fatalf("expected error for %q, got nil", in)
			}
		})
	}
}

// TestAuditFix_FUZZ1_SafeValidateGatewayKey_AcceptsWellFormedKey makes sure
// the guard does not regress against legitimate Hive pubkey strings.
func TestAuditFix_FUZZ1_SafeValidateGatewayKey_AcceptsWellFormedKey(t *testing.T) {
	// Same fixture used by hivego's own TestDecodePublicKey.
	good := "STM7dzxQo2aaav9weydSVAwqewcUz2GbUwyWrAVqkdiKsD6V1uX8B"
	if err := safeValidateGatewayKey(good); err != nil {
		t.Fatalf("safeValidateGatewayKey(%q) errored on a well-formed key: %v", good, err)
	}
}
