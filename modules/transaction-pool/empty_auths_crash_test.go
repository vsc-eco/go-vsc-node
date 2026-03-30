package transactionpool

import (
	"fmt"
	"testing"
)

// TestHashKeyAuths_EmptySlicePanic proves that HashKeyAuths panics when given
// an empty RequiredAuths slice. This is the root cause of a node crash:
//
// An attacker submits a transaction with RequiredAuths: [] (empty).
// IngestTx calls HashKeyAuths(txShell.Headers.RequiredAuths) at line 124.
// HashKeyAuths checks len(keyAuths) < 2, which is true for len 0,
// then does keyAuths[0] — index out of range panic — crashing the node.
//
// Additionally, even if HashKeyAuths were fixed, IngestTx line 171 does:
//   tp.rcs.GetAvailableRCs(txShell.Headers.RequiredAuths[0], latestBlk)
// which would also panic on an empty slice.
//
// Both are reachable from network input (P2P ReceiveTx and RPC IngestTx).
func TestHashKeyAuths_EmptySlicePanic(t *testing.T) {
	t.Log("=== ATTACK TEST: Empty RequiredAuths crashes VSC node ===")
	t.Log("Calling HashKeyAuths([]string{}) — this accesses [0] on an empty slice")

	panicked := false
	panicValue := ""

	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				panicValue = fmt.Sprintf("%v", r)
			}
		}()
		// This is the exact call made by IngestTx at line 124:
		//   hashAuths := HashKeyAuths(txShell.Headers.RequiredAuths)
		// When RequiredAuths is empty, this panics.
		_ = HashKeyAuths([]string{})
	}()

	if panicked {
		t.Logf("CONFIRMED CRASH: HashKeyAuths([]string{}) panicked with: %s", panicValue)
		t.Log("")
		t.Log("IMPACT: Any network peer can crash a VSC node by sending a")
		t.Log("transaction with empty RequiredAuths. This is a denial-of-service")
		t.Log("vulnerability reachable via P2P (ReceiveTx) and RPC (IngestTx).")
		t.Log("")
		t.Log("Affected code paths:")
		t.Log("  1. transaction-pool.go:124 — HashKeyAuths(txShell.Headers.RequiredAuths)")
		t.Log("  2. transaction-pool.go:171 — txShell.Headers.RequiredAuths[0]")
		t.Log("  3. transaction-pool.go:320 — txShell.Headers.RequiredAuths[0] (P2P path)")
		t.Log("")
		t.Log("FIX: Add len(RequiredAuths) == 0 check at the top of IngestTx/ReceiveTx")
		t.Log("     and in HashKeyAuths before accessing index 0.")
		// Test passes — we proved the crash exists
	} else {
		t.Log("HashKeyAuths handled empty input gracefully (no panic)")
		t.Log("The vulnerability may have been patched.")
	}
}

// TestHashKeyAuths_NilSlicePanic proves that a nil RequiredAuths also crashes.
func TestHashKeyAuths_NilSlicePanic(t *testing.T) {
	t.Log("=== ATTACK TEST: nil RequiredAuths crashes VSC node ===")

	panicked := false
	panicValue := ""

	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				panicValue = fmt.Sprintf("%v", r)
			}
		}()
		_ = HashKeyAuths(nil)
	}()

	if panicked {
		t.Logf("CONFIRMED CRASH: HashKeyAuths(nil) panicked with: %s", panicValue)
	} else {
		t.Log("HashKeyAuths handled nil input gracefully (no panic)")
	}
}

// TestHashKeyAuths_SingleElement verifies normal operation with one auth (no panic).
func TestHashKeyAuths_SingleElement(t *testing.T) {
	result := HashKeyAuths([]string{"did:key:z6Mktest"})
	if result != "did:key:z6Mktest" {
		t.Fatalf("expected did:key:z6Mktest, got %s", result)
	}
	t.Log("Single element works correctly — returns the key directly")
}
