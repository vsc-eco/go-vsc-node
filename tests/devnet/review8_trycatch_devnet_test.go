package devnet

// review8 — multi-node devnet proof of the try/catch inter-contract primitive.
//
// A try-call that catches a callee revert + rolls back its state must be
// DETERMINISTIC across every validator: if it weren't, the nodes would compute
// different state roots and the chain would stall (no 2/3 agreement). This test
// fires a real try-call whose callee writes state then aborts, and asserts every
// node converges on the same outcome (callee write rolled back, caller write
// committed) and the chain keeps advancing.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestReview8_TryCatchDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet try/catch test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)

	// Build the call-tss contract (carries setThenAbort + tryThenSet) before
	// bring-up so the Docker TinyGo build doesn't run under load.
	wasm, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
	}

	cfg := DefaultConfig()
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	t.Logf("starting %d-node devnet (~12 min)...", cfg.Nodes)
	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}

	// Bring-up: chain producing + first election indexed.
	if err := d.WaitForBlockProcessing(ctx, 1, 30, 8*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}
	if err := d.waitForElectionEpoch(ctx, 1, 1, 10*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("first election never indexed: %v", err)
	}

	// Deploy ONE contract; it try-calls ITSELF (caller and callee share the same
	// ContractSession — the hardest savepoint case: the rollback must drop the
	// callee's write while keeping the caller's pre/post-call writes). Using one
	// instance also avoids a second stop/restart deploy (a flaky devnet step).
	c, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "trycatch", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy contract: %v", err)
	}
	t.Logf("deployed contract=%s (self-calling)", c)
	if err := d.WaitForBlockProcessing(ctx, 2, nodeBlock(t, d, ctx, 1), 3*time.Minute); err != nil {
		t.Fatalf("nodes not synced post-deploy: %v", err)
	}

	// --- Case 1: self-call writes state then aborts -> caught + rolled back ---
	// tryThenSet payload: "calleeId;method;callPayload;markerKey;markerVal"
	if _, err := d.CallContract(ctx, 3, c, "tryThenSet",
		c+";setThenAbort;doomed,written;marker;continued"); err != nil {
		t.Fatalf("tryThenSet (catch) call: %v", err)
	}

	// Verified on a DIFFERENT node than the caller (node 4): if every node didn't
	// process this identically the value would never appear (chain would stall).
	if !waitStateKey(t, d, ctx, 4, c, "marker", "continued", 3*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("marker never committed — try-call did not catch, or nodes diverged")
	}

	// --- Case 2: self-call succeeds -> commits normally through the try-call ---
	if _, err := d.CallContract(ctx, 3, c, "tryThenSet",
		c+";setString;kept,value;marker2;done2"); err != nil {
		t.Fatalf("tryThenSet (success) call: %v", err)
	}
	if !waitStateKey(t, d, ctx, 4, c, "kept", "value", 3*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("successful inner write must persist through a try-call")
	}
	if !waitStateKey(t, d, ctx, 4, c, "marker2", "done2", 2*time.Minute) {
		t.Fatal("marker2 not committed on the success path")
	}

	// The Case-1 inner write ("doomed") must have been rolled back: the contract's
	// committed state holds marker/kept/marker2 but must NEVER contain "doomed".
	if st, err := d.GetStateByKeys(ctx, 4, c, []string{"doomed", "kept"}); err != nil {
		t.Errorf("read contract state: %v", err)
	} else {
		// GetStateByKeys returns absent keys as a nil value; a rolled-back write
		// is therefore nil (or empty). Fail only on a real surviving value.
		if v := st["doomed"]; v != nil && fmt.Sprintf("%v", v) != "" {
			t.Errorf("DX-H6 rollback failed: callee 'doomed' write survived, got %q", fmt.Sprintf("%v", v))
		}
		if got := fmt.Sprintf("%v", st["kept"]); got != "value" {
			t.Errorf("callee 'kept' should be 'value', got %q", got)
		}
	}

	// Chain must keep advancing on every node after the try-calls (consensus healthy).
	head := nodeBlock(t, d, ctx, 1)
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.WaitForBlockProcessing(ctx, n, head+5, 5*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d stalled after try-calls (possible non-determinism): %v", n, err)
		}
	}
	t.Log("try/catch is deterministic across all nodes; chain advanced cleanly")
}
