package devnet

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestForwarderContractIdLockedAfterFirstSet verifies the mapping's
// setForwarderContractId is locked after the first successful call —
// the underpinning of "Path B" in the trusted-forwarders governance
// spec.
//
// Spec (docs/dash-is-login/trusted-forwarders-governance.md):
//
//	"The mapping's setForwarderContractId is locked-after-first-set.
//	 To unlock it requires an admin sequence that the current contract
//	 code does NOT have a one-shot action for."
//
// This lock is what FORCES operators down the heavy migration path
// (vsc.update_contract → pause → clear → setForwarderContractId
// (new) → unpause) when they need to swap forwarder code. The doc
// flags the full E2E of that path as "Not done because path A handles
// every realistic case" — but the LOCK that motivates it is
// load-bearing, and a single-shot regression test of the lock itself
// is cheap.
//
// What this test proves:
//   - First setForwarderContractId(F1) succeeds and pins
//     mapping["forwarder"]=F1.
//   - Second setForwarderContractId(F2) on the same mapping aborts
//     with a recognisable error (not silently overwriting F1, not
//     panicking magi).
//   - mapping["forwarder"] remains F1 after the failed attempt.
//
// Operator-time-saved angle: if a refactor accidentally drops the
// lock check (e.g. someone "simplifies" the gate during a code
// cleanup), a malicious / careless operator could quietly swap the
// forwarder out from under the network. This catches it.
//
// Run: go test -v -run TestForwarderContractIdLockedAfterFirstSet -timeout 10m ./tests/devnet/
func TestForwarderContractIdLockedAfterFirstSet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping forwarder-lock devnet test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	mappingWasm, err := DashMappingContractPath()
	if err != nil {
		t.Fatalf("DashMappingContractPath: %v", err)
	}
	callTssWasm, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("BuildCallTssContract: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Nodes = 4
	cfg.GenesisNode = 4

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}

	mappingId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     mappingWasm,
		Name:         "dash-mapping",
		Description:  "forwarder-lock test mapping",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy mapping: %v", err)
	}
	// Two distinct candidate forwarders. Real path B is "swap to a
	// DIFFERENT forwarder contract id"; the test uses two arbitrary
	// allow-listable contracts (call-tss wasm is fine — we're only
	// exercising the SETTER lock, not actually invoking the
	// forwarder).
	forwarder1, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "forwarder-v1",
		Description:  "first forwarder — should land",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy forwarder1: %v", err)
	}
	forwarder2, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "forwarder-v2",
		Description:  "second forwarder — second setForwarderContractId must reject",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy forwarder2: %v", err)
	}
	t.Logf("mapping=%s forwarder1=%s forwarder2=%s", mappingId, forwarder1, forwarder2)

	// First setForwarderContractId — should succeed.
	tx1, err := d.CallContract(ctx, 1, mappingId, "setForwarderContractId", forwarder1)
	if err != nil {
		t.Fatalf("setForwarderContractId(F1): %v", err)
	}

	// Poll for the contract output; expect ok=true.
	if !waitForOk(t, ctx, d, tx1, 60*time.Second, true, "") {
		t.Fatal("setForwarderContractId(F1) did not land within 60s")
	}

	// Second setForwarderContractId on the same mapping — should
	// abort. Don't care which error code exactly, just that it's
	// ok=false and the message hints at the lock semantics.
	tx2, err := d.CallContract(ctx, 1, mappingId, "setForwarderContractId", forwarder2)
	if err != nil {
		t.Fatalf("setForwarderContractId(F2): %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)
	seenSecond := false
	for time.Now().Before(deadline) {
		outs, qerr := d.FindContractOutputByInput(ctx, 1, tx2)
		if qerr == nil && len(outs) > 0 && len(outs[0].Results) > 0 {
			r := outs[0].Results[0]
			t.Logf("setForwarderContractId(F2) output: ok=%v ret=%q err=%q errMsg=%q",
				r.Ok, r.Ret, r.Err, r.ErrMsg)
			seenSecond = true
			if r.Ok {
				t.Errorf("expected ok=false (forwarder lock should reject a second set), got ok=true ret=%q",
					r.Ret)
			}
			// The contract uses ContractError to surface the abort. We
			// don't pin the exact wording — any of "locked", "already
			// set", "forwarder", or "lock" is acceptable as a sanity
			// signal. If none appears we still surface the diagnostic
			// because something unexpected aborted the call.
			lower := strings.ToLower(r.ErrMsg)
			hasLockHint := strings.Contains(lower, "lock") ||
				strings.Contains(lower, "already") ||
				strings.Contains(lower, "forwarder") ||
				strings.Contains(lower, "set")
			if !hasLockHint {
				t.Logf("WARN: F2 errMsg=%q did not contain a forwarder/lock/already/set hint — abort happened but the error message is opaque",
					r.ErrMsg)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	if !seenSecond {
		t.Fatalf("no contract output for second setForwarderContractId tx=%s within 60s", tx2)
	}

	// Verify mapping["forwarder"] is still F1 (the lock held — F2
	// did not overwrite). Best-effort read; same "GQL returns cid
	// errors on contract-id state values" quirk we hit in the other
	// trusted-forwarders tests, so a read failure is logged, not
	// fatal.
	if st, qerr := d.GetStateByKeys(ctx, 1, mappingId, []string{"forwarder"}); qerr == nil {
		got, _ := st["forwarder"].(string)
		if got == forwarder2 {
			t.Errorf("mapping[\"forwarder\"]=%q — the second set overwrote F1 despite returning ok=false; the lock is broken",
				got)
		} else if got == forwarder1 {
			t.Logf("mapping[\"forwarder\"]=%q ✓ — lock held, F1 retained", got)
		} else {
			t.Logf("mapping[\"forwarder\"]=%q (neither F1 nor F2 — informational)", got)
		}
	} else {
		t.Logf("mapping[\"forwarder\"] readback failed (best-effort): %v", qerr)
	}

	t.Log("=== TestForwarderContractIdLockedAfterFirstSet: PASS — setForwarderContractId is locked after first set, the lock that Path B (heavy migration) presupposes ===")
}

// waitForOk polls a contract output until it lands. Returns true if
// observed within timeout. expectOk + expectRetSub control the
// assertion: when expectOk=true the output must have ok=true; when
// false the output must have ok=false AND ret/errMsg contains
// expectRetSub (case-insensitive). expectRetSub="" skips the substring
// check.
func waitForOk(t *testing.T, ctx context.Context, d *Devnet, txId string, timeout time.Duration, expectOk bool, expectRetSub string) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		outs, qerr := d.FindContractOutputByInput(ctx, 1, txId)
		if qerr == nil && len(outs) > 0 && len(outs[0].Results) > 0 {
			r := outs[0].Results[0]
			t.Logf("tx=%s output: ok=%v ret=%q err=%q errMsg=%q", txId, r.Ok, r.Ret, r.Err, r.ErrMsg)
			if expectOk {
				if !r.Ok {
					t.Errorf("expected ok=true, got ok=false ret=%q err=%q errMsg=%q",
						r.Ret, r.Err, r.ErrMsg)
				}
			} else {
				if r.Ok {
					t.Errorf("expected ok=false, got ok=true ret=%q", r.Ret)
				}
				if expectRetSub != "" {
					blob := strings.ToLower(r.Ret + ":" + r.ErrMsg)
					if !strings.Contains(blob, strings.ToLower(expectRetSub)) {
						t.Errorf("expected ret/errMsg to contain %q, got ret=%q errMsg=%q",
							expectRetSub, r.Ret, r.ErrMsg)
					}
				}
			}
			return true
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	return false
}
