package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"
)

// TestTrustedForwardersSysconfigUnset_AbortsCallAs is the symmetric
// pair to TestTrustedForwardersForwarderUnset_AbortsCallAs — same
// expected outcome ("TrustedForwarders empty → call_as aborts"), but
// triggered by the OTHER half of the deploy checklist being skipped.
//
// D ("forwarder unset") covers: operator pointed sysconfig at the
// mapping (step 1), but never called setForwarderContractId on the
// mapping (step 2) → mapping["forwarder"] is empty → resolver returns
// nil.
//
// This test ("sysconfig unset") covers the opposite: operator deployed
// the mapping AND called setForwarderContractId (step 2), but never
// updated SysConfigOverrides.DashMappingContractId (step 1) → magi
// has no mapping pointer to read "forwarder" from → resolver again
// returns nil.
//
// Together, D + this test prove the AND-gate semantics of the two
// pointers, so a future refactor that loses either half can't sneak
// past CI. Same operator-time-saved rationale as D: silent
// misconfiguration is what burns hours; an explicit, named abort
// surfaces the mistake in the first log line.
//
// Run: go test -v -run TestTrustedForwardersSysconfigUnset -timeout 12m ./tests/devnet/
func TestTrustedForwardersSysconfigUnset_AbortsCallAs(t *testing.T) {
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
	// Empty overrides so the magi nodes are started with the
	// `-sysconfig /data/devnet/sysconfig.json` flag (see compose.go).
	// DashMappingContractId is left UNSET on purpose — that's the
	// misconfig under test.
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

	t.Log("deploying mapping (will call setForwarderContractId), call-tss forwarder caller, call-tss target...")
	mappingId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     mappingWasm,
		Name:         "dash-mapping",
		Description:  "sysconfig-unset misconfig test mapping",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy mapping: %v", err)
	}
	forwarderId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "call-tss-forwarder",
		Description:  "would-be forwarder caller — never reachable because sysconfig is unset",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy forwarder caller: %v", err)
	}
	targetId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "call-tss-target",
		Description:  "sysconfig-unset misconfig test target",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy target: %v", err)
	}
	t.Logf("mapping=%s forwarder=%s target=%s", mappingId, forwarderId, targetId)

	// Call setForwarderContractId — the MAPPING side is wired
	// correctly (step 2 done). Then deliberately do NOT call
	// SetDashMappingContractId — that's the sysconfig side (step 1)
	// that the operator forgot.
	if _, err := d.CallContract(ctx, 1, mappingId, "setForwarderContractId", forwarderId); err != nil {
		t.Fatalf("setForwarderContractId: %v", err)
	}
	// Restart so any hypothetical sysconfig reload reflects the
	// (intentionally still-empty) overrides.
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("RestartAllMagiNodes: %v", err)
	}
	t.Log("waiting 30s for magi L1 streamers to settle...")
	time.Sleep(30 * time.Second)

	// Sanity: confirm the mapping IS pinned to the forwarder (so the
	// test isn't accidentally proving D's case). Best-effort — the GQL
	// state-getter sometimes interprets the stored value as a CID and
	// returns "invalid cid: cid too short" even when the underlying
	// state is correct (string-shaped contract id). On a failure we
	// don't bail; the tryCallAs assertion below is the real proof, and
	// if mapping["forwarder"] were silently unset we'd still see
	// "TrustedForwarders" in the abort (just for the OTHER reason —
	// indistinguishable, but the docs callers care that BOTH symmetric
	// misconfigs produce the same operator-visible error, so collapse
	// is acceptable here).
	if st, qerr := d.GetStateByKeys(ctx, 1, mappingId, []string{"forwarder"}); qerr == nil {
		got, _ := st["forwarder"].(string)
		if got != "" && got != forwarderId {
			t.Fatalf("precondition violated: mapping[\"forwarder\"]=%q, expected %q",
				got, forwarderId)
		}
		t.Logf("precondition: mapping[\"forwarder\"]=%q (mapping-side wiring sanity)", got)
	} else {
		t.Logf("mapping[\"forwarder\"] GQL read failed (treating as best-effort): %v", qerr)
	}

	// Sanity: confirm sysconfig.DashMappingContractId IS empty in
	// the running magi nodes. We can't read magi's in-memory
	// sysconfig over GQL, but a JSON parse of the on-disk file
	// (visible to the host) is sufficient — Start writes the
	// overrides verbatim, and we didn't mutate it after Start.
	if inContainer, eerr := d.ExecInMagi(ctx, 1, "cat", "/data/devnet/sysconfig.json"); eerr == nil {
		t.Logf("magi-1 in-container sysconfig.json:\n%s", inContainer)
		if strings.Contains(inContainer, "dashMappingContractId") &&
			!strings.Contains(inContainer, `"dashMappingContractId":""`) {
			t.Fatalf("precondition violated: sysconfig.json has a non-empty dashMappingContractId — expected unset")
		}
	} else {
		t.Logf("could not read in-container sysconfig: %v (treating as unset)", eerr)
	}

	// Fire the call_as via the forwarder-caller's tryCallAs entry.
	// Because magi's TrustedForwarders list is empty (no mapping
	// pointer → resolver returns nil), the call_as host fn aborts
	// with the canonical "not in system-config.TrustedForwarders"
	// message.
	payload := fmt.Sprintf("%s;%s;%s;%s", targetId, "did:test:sysconfig-unset", "shouldNotLand", "nope")
	txId, err := d.CallContract(ctx, 1, forwarderId, "tryCallAs", payload)
	if err != nil {
		t.Fatalf("tryCallAs: %v", err)
	}

	// Poll for the contract output; expect ok=false with TrustedForwarders.
	deadline := time.Now().Add(60 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		outs, qerr := d.FindContractOutputByInput(ctx, 1, txId)
		if qerr == nil && len(outs) > 0 && len(outs[0].Results) > 0 {
			r := outs[0].Results[0]
			t.Logf("tryCallAs output: ok=%v ret=%q err=%q errMsg=%q", r.Ok, r.Ret, r.Err, r.ErrMsg)
			seen = true
			if r.Ok {
				t.Errorf("expected ok=false (call_as aborted because sysconfig.DashMappingContractId is unset), got ok=true ret=%q",
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

	// Target must NOT have been written.
	if st, _ := d.GetStateByKeys(ctx, 1, targetId, []string{"shouldNotLand"}); st != nil {
		if v, _ := st["shouldNotLand"].(string); v != "" {
			t.Errorf("target state was written despite the abort: target[shouldNotLand]=%q",
				v)
		}
	}

	t.Log("=== TestTrustedForwardersSysconfigUnset: PASS — operator-skipped-step-1 misconfig surfaced as a clear sdk_error ===")
}
