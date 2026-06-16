package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"
)

// TestTrustedForwardersEmergencyRevoke verifies that clearing
// sysconfig.DashMappingContractId at runtime disables the call_as
// privilege on the affected witness — the "emergency revoke via
// sysconfig clear" path documented in docs/dash-is-login/trusted-
// forwarders-governance.md's "Emergency revoke" section.
//
// This is the only kill-switch operators have for IS-login compromise
// response. The doc claims:
//
//	"Set sysconfig.DashMappingContractId="" on the affected witness +
//	 restart. resolveTrustedForwarders now returns an empty list →
//	 every call_as via Dash IS-login aborts on that witness."
//
// This test exercises that claim end-to-end against a real magi node:
// deploy + wire the full Dash IS-login chain (forwarder pinned in the
// mapping, sysconfig pointing at the mapping), verify call_as
// succeeds, clear sysconfig.DashMappingContractId + restart magi,
// verify the same call_as now aborts.
//
// Bypasses the IS-login attestation + Dash payment chain entirely by
// driving sdk.ContractCallAs directly from a call-tss instance pinned
// as the forwarder. The call-tss "tryCallAs" wasmexport (added in the
// same commit as this test) wraps ContractCallAs with no other
// preconditions, so the test signal is purely "is the call_as gate
// open on this witness right now?"
//
// Cost: 2x devnet restart cycles + 2 contract calls; total ~5 min.
// Run: go test -v -run TestTrustedForwardersEmergencyRevoke -timeout 15m ./tests/devnet/
func TestTrustedForwardersEmergencyRevoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	// Reuse call-tss as both the "forwarder" (calls ContractCallAs) and
	// the "target" (records the state write via setString). Same wasm
	// instance, two deploys.
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
	// Initialise empty SysConfigOverrides so the magi nodes are started
	// with the `-sysconfig /data/devnet/sysconfig.json` flag at boot
	// (compose generation captures whether overrides are non-nil at
	// start time — see tests/devnet/compose.go). Without this, the
	// later SetDashMappingContractId writes the JSON file but the
	// running magid binary ignores it because it wasn't given the
	// flag. Same gotcha that bit TestIsLoginOpCallSmoke previously.
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

	// ===== Deploy =====
	t.Log("deploying dash-mapping...")
	mappingId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     mappingWasm,
		Name:         "dash-mapping",
		Description:  "emergency-revoke test mapping",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploying mapping: %v", err)
	}
	t.Logf("mapping: %s", mappingId)

	t.Log("deploying call-tss as forwarder + as target...")
	forwarderId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "call-tss-forwarder",
		Description:  "emergency-revoke test pinned forwarder",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploying forwarder: %v", err)
	}
	t.Logf("forwarder: %s", forwarderId)

	targetId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "call-tss-target",
		Description:  "emergency-revoke test target",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploying target: %v", err)
	}
	t.Logf("target: %s", targetId)

	// ===== Wire the sysconfig → mapping pointer + restart magi =====
	//
	// Sysconfig must be set BEFORE the magi restart (it's only read at
	// startup). The mapping's "forwarder" state key is set AFTER the
	// restart — the opcall test follows the same ordering, and putting
	// the contract call before the restart loses the in-flight L1 tx
	// during the --force-recreate. After restart we wait 30s for the
	// L1 streamer to settle, THEN broadcast setForwarderContractId,
	// THEN poll for its contract output to confirm the "forwarder"
	// state key was populated before phase A proceeds.
	if err := d.SetDashMappingContractId(mappingId); err != nil {
		t.Fatalf("SetDashMappingContractId(%s): %v", mappingId, err)
	}
	t.Log("restarting magi nodes to pick up sysconfig...")
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("RestartAllMagiNodes: %v", err)
	}
	t.Log("waiting 30s for magi L1 streamers to settle post-restart...")
	time.Sleep(30 * time.Second)

	setForwarderTxId, err := d.CallContract(ctx, 1, mappingId, "setForwarderContractId", forwarderId)
	if err != nil {
		t.Fatalf("setForwarderContractId: %v", err)
	}
	if err := waitForRevokeContractOutput(ctx, t, d, setForwarderTxId, "setForwarderContractId", true); err != nil {
		t.Fatal(err)
	}

	// Diagnostics: verify magi sees the same state we think we set up.
	// If phase A fails despite all the contract outputs being green,
	// one of these will tell us why.
	if inContainer, eerr := d.ExecInMagi(ctx, 1, "cat", "/data/devnet/sysconfig.json"); eerr == nil {
		t.Logf("sysconfig.json (in-container, magi-1):\n%s", inContainer)
	} else {
		t.Logf("magi-1 sysconfig.json cat failed: %v", eerr)
	}
	if cmdline, cerr := d.ExecInMagi(ctx, 1, "sh", "-c", "tr '\\0' ' ' < /proc/1/cmdline"); cerr == nil {
		t.Logf("magi-1 PID 1 cmdline: %s", cmdline)
	}
	// Read the mapping's "forwarder" state key via GQL — this is the
	// EXACT same key magi reads in resolveTrustedForwarders. If this
	// returns empty or the wrong id, we know the chain side is wrong.
	// If it returns the right id, the issue is on magi's read side.
	if st, qerr := d.GetStateByKeys(ctx, 1, mappingId, []string{"forwarder"}); qerr == nil {
		got, _ := st["forwarder"].(string)
		t.Logf("mapping[\"forwarder\"] via GQL: %q (expected %q)", got, forwarderId)
		if got != forwarderId {
			t.Fatalf("setForwarderContractId did not persist; mapping[\"forwarder\"]=%q expected=%q", got, forwarderId)
		}
	} else {
		t.Logf("GQL getStateByKeys for mapping[\"forwarder\"] failed: %v", qerr)
	}
	t.Logf("mapping[\"forwarder\"] pinned to %s", forwarderId)

	// ===== Phase A: gate open — call_as MUST succeed =====
	t.Log("=== phase A: gate OPEN (sysconfig points at mapping, mapping pins forwarder) ===")
	const keyA = "phaseA"
	const valA = "open"
	const userDID = "did:test:emergencyrevoke"

	openPayload := fmt.Sprintf("%s;%s;%s;%s", targetId, userDID, keyA, valA)
	openTxId, err := d.CallContract(ctx, 1, forwarderId, "tryCallAs", openPayload)
	if err != nil {
		t.Fatalf("tryCallAs (gate open): %v", err)
	}
	t.Logf("phase A tryCallAs L1 tx=%s", openTxId)
	if err := waitForRevokeContractOutput(ctx, t, d, openTxId, "phase A tryCallAs", true); err != nil {
		t.Fatal(err)
	}
	// Verify the target's state actually changed (proves call_as
	// reached the target with effectiveCaller=did:test:..., not just
	// "no error from the forwarder").
	targetVal := pollRevokeStateKey(ctx, t, d, targetId, keyA, 30*time.Second)
	if targetVal != valA {
		t.Fatalf("phase A: expected target[%q]=%q (proves call_as completed), got %q", keyA, valA, targetVal)
	}
	t.Logf("phase A verified: target[%q]=%q via call_as", keyA, targetVal)

	// ===== Emergency revoke: clear sysconfig + restart =====
	t.Log("=== emergency revoke: clearing sysconfig.DashMappingContractId + restarting magi ===")
	if err := d.SetDashMappingContractId(""); err != nil {
		t.Fatalf("SetDashMappingContractId(\"\"): %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("RestartAllMagiNodes post-clear: %v", err)
	}
	t.Log("waiting 30s for magi L1 streamers to settle post-restart...")
	time.Sleep(30 * time.Second)

	// ===== Phase B: gate closed — call_as MUST abort =====
	t.Log("=== phase B: gate CLOSED (sysconfig cleared) — call_as must abort ===")
	const keyB = "phaseB"
	const valB = "should-not-land"

	closedPayload := fmt.Sprintf("%s;%s;%s;%s", targetId, userDID, keyB, valB)
	closedTxId, err := d.CallContract(ctx, 1, forwarderId, "tryCallAs", closedPayload)
	if err != nil {
		t.Fatalf("tryCallAs (gate closed): %v", err)
	}
	t.Logf("phase B tryCallAs L1 tx=%s", closedTxId)
	// Expecting FAILURE — the forwarder's tryCallAs will receive the
	// host's "not in TrustedForwarders" error from contractCallAs and
	// either bubble it up (Ok=false at the magi level) or return the
	// error string as its result. Either shape is acceptable for the
	// security claim; we just need to verify the target's state
	// DIDN'T change.
	if err := waitForRevokeContractOutput(ctx, t, d, closedTxId, "phase B tryCallAs", false); err != nil {
		// Logged but not fatal — the precise error-shape varies (Ok
		// false vs. Ok true with the error string in Ret). The
		// authoritative check is the target's state below.
		t.Logf("phase B output (informational): %v", err)
	}
	// Defensive: confirm the gate is closed by giving magi time to
	// commit the failure, then check that the target state is STILL
	// empty for keyB.
	time.Sleep(8 * time.Second)
	gotB, _ := d.GetStateByKeys(ctx, 1, targetId, []string{keyB})
	if v, _ := gotB[keyB].(string); v != "" {
		t.Errorf("phase B: target[%q]=%q — emergency revoke FAILED to disable call_as (gate stayed open)",
			keyB, v)
	} else {
		t.Logf("phase B verified: target[%q] is unset — call_as gate is closed (emergency revoke worked)", keyB)
	}

	// Sanity: phase A's state write should still be intact (revoke
	// doesn't roll back history, just blocks new privileged calls).
	stillA := pollRevokeStateKey(ctx, t, d, targetId, keyA, 5*time.Second)
	if stillA != valA {
		t.Errorf("phase A state should be untouched by revoke; got target[%q]=%q (want %q)",
			keyA, stillA, valA)
	}
	t.Log("=== TestTrustedForwardersEmergencyRevoke: PASS ===")
}

// waitForRevokeContractOutput polls FindContractOutputByInput until a
// result lands or 60s elapses. If wantOk is true, asserts the call
// succeeded; if false, just logs whatever shape came back (the precise
// shape of an aborted call_as varies — see the phase B caller's note).
func waitForRevokeContractOutput(ctx context.Context, t *testing.T, d *Devnet, txId, label string, wantOk bool) error {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		outs, err := d.FindContractOutputByInput(ctx, 1, txId)
		if err == nil && len(outs) > 0 && len(outs[0].Results) > 0 {
			r := outs[0].Results[0]
			t.Logf("%s output: ok=%v ret=%q err=%q errMsg=%q", label, r.Ok, r.Ret, r.Err, r.ErrMsg)
			if wantOk && !r.Ok {
				return fmt.Errorf("%s expected ok=true, got ok=%v err=%q errMsg=%q",
					label, r.Ok, r.Err, r.ErrMsg)
			}
			if !wantOk && r.Ok && !strings.Contains(r.Ret, "TrustedForwarders") && !strings.Contains(r.Ret, "call_as") {
				return fmt.Errorf("%s expected abort or call_as-failure ret, got ok=true ret=%q",
					label, r.Ret)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("%s contract output never landed for tx=%s within 60s", label, txId)
}

// pollRevokeStateKey polls a state key for a non-empty value; returns
// "" on timeout. Same shape as the helper in the governance test.
func pollRevokeStateKey(ctx context.Context, t *testing.T, d *Devnet, contractId, key string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := d.GetStateByKeys(ctx, 1, contractId, []string{key})
		if err == nil {
			if v, ok := st[key].(string); ok && v != "" {
				return v
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(2 * time.Second):
		}
	}
	return ""
}
