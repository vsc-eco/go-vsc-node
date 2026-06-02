package main

import (
	"strings"
	"testing"
)

// a well-formed signature is 65 bytes (130 hex chars): 1 header + r(32) + s(32).
var validSigHex = strings.Repeat("ab", 65)

func TestValidateSignatureHex(t *testing.T) {
	if err := validateSignatureHex(validSigHex); err != nil {
		t.Fatalf("valid 65-byte sig rejected: %v", err)
	}
	if err := validateSignatureHex(strings.Repeat("ab", 64)); err == nil {
		t.Fatal("expected error for wrong-length signature")
	}
	if err := validateSignatureHex("nothexnothex"); err == nil {
		t.Fatal("expected error for non-hex signature")
	}
}

func TestRunExternalSigner_HappyPath(t *testing.T) {
	// Consume stdin and emit a valid signature (with a trailing newline, which
	// must be trimmed).
	sig, err := runExternalSigner("cat >/dev/null; echo "+validSigHex, []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig != validSigHex {
		t.Fatalf("got %q want %q", sig, validSigHex)
	}
}

func TestRunExternalSigner_CommandNotFound(t *testing.T) {
	// Stands in for "the signer isn't installed": the deployer must surface an
	// error rather than broadcast anything.
	_, err := runExternalSigner("vsc-ledger-sign-definitely-missing-xyz", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error when signer command is missing")
	}
}

func TestRunExternalSigner_NonHexOutputRejected(t *testing.T) {
	_, err := runExternalSigner("echo not-a-signature", []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "invalid signature") {
		t.Fatalf("expected invalid-signature error, got %v", err)
	}
}

func TestRunExternalSigner_StderrSurfaced(t *testing.T) {
	// A signer that fails (e.g. no device, blind signing off) writes a reason to
	// stderr; that reason must reach the operator.
	_, err := runExternalSigner("echo 'enable blind signing' >&2; exit 5", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error when signer exits non-zero")
	}
	if !strings.Contains(err.Error(), "enable blind signing") {
		t.Fatalf("stderr not surfaced in error: %v", err)
	}
}
