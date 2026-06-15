package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"
)

// TestTrustedForwardersForwarderUnset_AbortsCallAs verifies the
// "operator forgot setForwarderContractId" deploy-time misconfiguration
// from the failure-modes table in
// docs/dash-is-login/trusted-forwarders-governance.md:
//
//	"Mapping deployed, setForwarderContractId not called →
//	 resolveTrustedForwarders returns nil (state key empty) → call_as
//	 aborts. Safe default."
//
// Operator-time wasted if magi panics, returns ok=true with a misleading
// message, or otherwise hides the misconfiguration: the operator deploys
// the mapping + points sysconfig at it, sees no obvious error, the
// IS-login feature silently does the wrong thing, and they spend hours
// in logs before realising they skipped step 2 of the deploy guide.
//
// This test covers the "missed step 2" case explicitly: deploy mapping,
// SetDashMappingContractId(mappingId), restart magi — but never call
// mapping.setForwarderContractId. Then invoke a contract that tries
// sdk.ContractCallAs. Expected behaviour: aborts with "caller
// contract:<id> is not in system-config.TrustedForwarders" (because
// the resolver returns an empty list when "forwarder" state key is
// missing), the call's contract output records ok=false + the error
// message, and there are NO panics anywhere in the magi node logs.
//
// Pairs with TestTrustedForwardersEmergencyRevoke (same misconfig
// surface, different cause): emergency-revoke clears the SYSCONFIG
// pointer, this one verifies the MAPPING side of the same gate.
//
// Run: go test -v -run TestTrustedForwardersForwarderUnset -timeout 12m ./tests/devnet/
func TestTrustedForwardersForwarderUnset_AbortsCallAs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()

	callTssWasm, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("BuildCallTssContract: %v", err)
	}
	mappingWasm, err := DashMappingContractPath()
	if err != nil {
		t.Fatalf("DashMappingContractPath: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Nodes = 4
	cfg.GenesisNode = 4
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}

	t.Log("deploying mapping (NO setForwarderContractId), forwarder, target...")
	mappingId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     mappingWasm,
		Name:         "dash-mapping",
		Description:  "misconfig test mapping (forwarder intentionally unset)",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy mapping: %v", err)
	}
	forwarderId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "call-tss-forwarder",
		Description:  "misconfig test caller — never pinned in mapping",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy forwarder: %v", err)
	}
	targetId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "call-tss-target",
		Description:  "misconfig test target",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy target: %v", err)
	}
	t.Logf("mapping=%s forwarder=%s target=%s", mappingId, forwarderId, targetId)

	// Point sysconfig at the mapping but DELIBERATELY don't call
	// setForwarderContractId. This is the misconfig under test —
	// operator did step 1 (deploy + sysconfig) but forgot step 2
	// (pin the forwarder).
	if err := d.SetDashMappingContractId(mappingId); err != nil {
		t.Fatalf("SetDashMappingContractId: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("RestartAllMagiNodes: %v", err)
	}
	t.Log("waiting 30s for magi L1 streamers to settle...")
	time.Sleep(30 * time.Second)

	// Sanity: confirm mapping["forwarder"] really IS unset before we
	// try to call_as. If somehow the contract pre-populated it, the
	// test isn't proving what we think.
	if st, qerr := d.GetStateByKeys(ctx, 1, mappingId, []string{"forwarder"}); qerr == nil {
		if got, _ := st["forwarder"].(string); got != "" {
			t.Fatalf("precondition violated: mapping[\"forwarder\"]=%q, expected unset",
				got)
		}
		t.Log("precondition: mapping[\"forwarder\"] is unset ✓")
	} else {
		t.Logf("mapping[\"forwarder\"] GQL read failed (acceptable, treating as unset): %v", qerr)
	}

	// Try the call_as. Should abort.
	payload := fmt.Sprintf("%s;%s;%s;%s", targetId, "did:test:misconfig", "shouldNotLand", "nope")
	txId, err := d.CallContract(ctx, 1, forwarderId, "tryCallAs", payload)
	if err != nil {
		t.Fatalf("tryCallAs: %v", err)
	}

	// Poll for the contract output; expect ok=false with the canonical
	// "not in system-config.TrustedForwarders" error.
	deadline := time.Now().Add(60 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		outs, qerr := d.FindContractOutputByInput(ctx, 1, txId)
		if qerr == nil && len(outs) > 0 && len(outs[0].Results) > 0 {
			r := outs[0].Results[0]
			t.Logf("tryCallAs output: ok=%v ret=%q err=%q errMsg=%q", r.Ok, r.Ret, r.Err, r.ErrMsg)
			seen = true
			if r.Ok {
				t.Errorf("expected ok=false (call_as aborted because mapping[\"forwarder\"] is unset), got ok=true ret=%q",
					r.Ret)
			}
			if !strings.Contains(r.ErrMsg, "TrustedForwarders") {
				t.Errorf("expected errMsg to contain 'TrustedForwarders', got: %q", r.ErrMsg)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	if !seen {
		t.Fatalf("no contract output landed for tx=%s within 60s", txId)
	}

	// Confirm target's state was NOT written (the abort happened
	// before the call would have reached setString).
	if st, _ := d.GetStateByKeys(ctx, 1, targetId, []string{"shouldNotLand"}); st != nil {
		if v, _ := st["shouldNotLand"].(string); v != "" {
			t.Errorf("target state was written despite the abort: target[shouldNotLand]=%q",
				v)
		}
	}

	t.Log("=== TestTrustedForwardersForwarderUnset: PASS — operator-skipped-step-2 misconfig surfaced as a clear sdk_error ===")
}
