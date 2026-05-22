package tss_test

// Audit finding #33 (unfixed): the TSS pipeline has no failure-counter or
// key-freeze policy. When a signature fails verification at
// state_engine.go:979 (btcec) or :992 (ed25519), the code simply
// continues; the key remains active and may be asked to sign again
// indefinitely. There is no MAX_SIGN_FAILURES_BEFORE_FREEZE constant,
// no per-key failure counter on TssKey, and no consumer of such a
// counter anywhere in modules/.
//
// Reachability: any compromised or malfunctioning party can keep
// triggering signing rounds with no economic or operational
// disincentive, eventually exhausting key material lifetime or harming
// liveness for the affected wallet.
//
// This test demonstrates the *precondition* by scanning the source for
// the absence of any freeze/failure-counter knob. When the fix lands
// (introducing e.g. `TSS_MAX_SIGN_FAILURES_BEFORE_FREEZE` or a similar
// const + counter), the assertions below will trip — at that point
// update this test to assert the new bound directly.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAuditUnfixed_33_NoTssFailureCounterOrFreezePolicy(t *testing.T) {
	// Walk modules/ and grep for any of the expected freeze/failure
	// counter token names. The audit recommends one of these be added;
	// today none exist.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller file")
	}
	modulesDir := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	// thisFile = .../modules/tss/audit_unfixed_33_test.go
	// modulesDir wants to be .../modules
	modulesDir = filepath.Join(filepath.Dir(thisFile), "..")
	abs, err := filepath.Abs(modulesDir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	modulesDir = abs

	// Token names the fix would plausibly introduce.
	wanted := []string{
		"MAX_SIGN_FAILURES_BEFORE_FREEZE",
		"TSS_MAX_SIGN_FAILURES",
		"SignFailureCounter",
		"FrozenAt", // a field on TssKey
		"KeyFrozen",
	}

	hits := map[string][]string{}
	err = filepath.Walk(modulesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // best-effort
		}
		if info.IsDir() {
			// skip vendored/non-source noise — there isn't really any
			// inside modules/ but be defensive.
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Allow this very test file to mention the tokens (otherwise
		// our own source would self-match).
		if strings.HasSuffix(path, "audit_unfixed_33_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		src := string(raw)
		for _, tok := range wanted {
			if strings.Contains(src, tok) {
				hits[tok] = append(hits[tok], path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(hits) != 0 {
		// Fix has likely landed.
		t.Fatalf("expected ZERO occurrences of any failure-counter / freeze token in modules/, but found: %+v", hits)
	}
	t.Logf("precondition confirmed: no TSS failure-counter or freeze policy exists in modules/")
}

// TestAuditUnfixed_33_VerifyFailurePathHasNoStateMutation reads
// state_engine.go around the btcec/ed25519 verify calls and asserts
// that the `if verified { ... }` block has NO companion `else` branch
// — i.e. the failed-verify path is silently dropped, not counted.
func TestAuditUnfixed_33_VerifyFailurePathHasNoStateMutation(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller file")
	}
	// thisFile = .../modules/tss/audit_unfixed_33_test.go
	statePath := filepath.Join(filepath.Dir(thisFile), "..", "state-processing", "state_engine.go")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state_engine.go: %v", err)
	}
	src := string(raw)

	// Find the btcec verify block and check there's no `} else {` after
	// the matching closing brace of `if verified {`.
	verifyMarker := "verified := signature.Verify(msgBytes, pubKey)"
	idx := strings.Index(src, verifyMarker)
	if idx < 0 {
		t.Fatalf("did not find btcec verify marker — file structure changed")
	}
	window := src[idx : idx+800]
	if !strings.Contains(window, "if verified {") {
		t.Fatalf("expected `if verified {` inside btcec window; window=%q", window)
	}
	// "} else {" inside this window would indicate a failure-path
	// counter was added.
	if strings.Contains(window, "} else {") {
		t.Fatalf("found `} else {` after verified-check — failure-counter may have been added: %q", window)
	}

	// Same check for the ed25519 path.
	edMarker := "edVerify := ed25519.Verify(pk, msgBytes, sigBytes)"
	idx2 := strings.Index(src, edMarker)
	if idx2 < 0 {
		t.Fatalf("did not find ed25519 verify marker — file structure changed")
	}
	window2 := src[idx2 : idx2+800]
	if strings.Contains(window2, "} else {") {
		t.Fatalf("found `} else {` after edVerify-check — failure-counter may have been added: %q", window2)
	}
	t.Logf("precondition confirmed: verify-failure path is silently dropped, no counter increment")
}
