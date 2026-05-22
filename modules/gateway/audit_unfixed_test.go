package gateway_test

// Audit finding FUZZ-1 (unfixed): hivego.DecodePublicKey panics with
// "slice bounds out of range [-4:]" when given a malformed prefix-only
// public key like "STM" (or any STM-prefixed string whose base58 body
// decodes to fewer than 4 bytes).
//
// Reachability: a malicious or misconfigured witness can publish such a
// value as its GatewayKey. The next gateway key-rotation
// (multisig.keyRotation -> hiveCreator.UpdateAccount ->
// hivego.UpdateAccountOperation.SerializeOp -> appendOptionalAuthority
// -> writePublicKey -> DecodePublicKey) will panic while serializing
// the rotation tx. The panic crashes the gateway goroutine and, with no
// recover() on that path, can DoS the witness/observer node.
//
// This test demonstrates the *precondition*: the underlying decoder
// panics on adversary-controlled input.  When the bug is fixed
// (DecodePublicKey returns an error on short inputs, or the gateway
// validates GatewayKey before passing it through), the panic will no
// longer occur and the recover() below will see nil — at which point
// this test must be updated to assert "no panic, returns error".

import (
	"testing"

	"github.com/vsc-eco/hivego"
)

// TestAuditUnfixed_FUZZ1_DecodePublicKeyPanics asserts the raw
// hivego.DecodePublicKey panics for "STM"-only input. This is the
// minimal repro the audit identified.
func TestAuditUnfixed_FUZZ1_DecodePublicKeyPanics(t *testing.T) {
	panicked := false
	var rec interface{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				rec = r
			}
		}()
		_, _ = hivego.DecodePublicKey("STM")
	}()
	if !panicked {
		// When fixed, DecodePublicKey will return an error instead of
		// panicking, and this branch will be taken — flip the assertion
		// at that point.
		t.Fatalf("expected DecodePublicKey(\"STM\") to panic, but it did not")
	}
	t.Logf("captured panic (precondition for FUZZ-1 reachable): %v", rec)
}

// TestAuditUnfixed_FUZZ1_DecodePublicKeyPanics_VariousShortInputs
// exercises a few prefix-only / short-payload variants to demonstrate
// that the panic surface is not a single magic value.
func TestAuditUnfixed_FUZZ1_DecodePublicKeyPanics_VariousShortInputs(t *testing.T) {
	cases := []string{"STM", "STM1", "STM11", "STM111"}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			panicked := false
			func() {
				defer func() {
					if r := recover(); r != nil {
						panicked = true
					}
				}()
				_, _ = hivego.DecodePublicKey(in)
			}()
			// We only require that at least one short-prefix input
			// panics; not every base58 short string decodes to <4
			// bytes. The "STM" case is the canonical repro.
			if in == "STM" && !panicked {
				t.Fatalf("expected panic for input %q", in)
			}
		})
	}
}
