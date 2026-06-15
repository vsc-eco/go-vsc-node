package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"
)

// TestTrustedForwardersGovernanceE2E covers the acceptance criteria from
// docs/dash-is-login/trusted-forwarders-governance.md:
//
//  1. Deploy the governance contract on devnet.
//  2. Pre-activation: propose + activate succeeds, but the magi execution
//     context still reads sysconfig only. (We can't easily downgrade the
//     consensus version on devnet — devnet auto-activates the binary's
//     currentConsensus — so we ASSERT this behaviour transitively: the
//     governance contract is at the same chain that runs at our
//     consensus version, so what we actually test is the post-activation
//     resolveTrustedForwarders union semantics.)
//  3. Standard propose → wait → activate sets the "active" key.
//  4. Standard propose-remove → wait → activate clears it.
//  5. emergencyRevoke clears immediately, no timelock.
//  6. setTimelock / setEmergencyRevokeAllowed admin paths.
//
// The full magi-side resolveTrustedForwarders behaviour (union +
// revocations) is covered by the unit tests in
// modules/state-processing/trusted_forwarders_test.go; the call_as
// integration is proven by TestIsLoginOpCallSmoke. This test is the
// chain-side E2E proof that the governance contract + magi state-read
// integration works end-to-end on a real devnet.
//
// Run: go test -v -run TestTrustedForwardersGovernanceE2E -timeout 25m ./tests/devnet/
func TestTrustedForwardersGovernanceE2E(t *testing.T) {
	govWasm, err := GovernanceTrustedForwardersContractPath()
	if err != nil {
		t.Fatalf("governance contract wasm: %v", err)
	}
	t.Logf("governance contract wasm: %s", govWasm)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	cfg := DefaultConfig()
	cfg.Nodes = 4
	cfg.GenesisNode = 4
	// Lower the contract's default timelock so the activate paths run
	// in reasonable test wall-clock. We set the override via the
	// contract's setTimelock action below; this just keeps the magi
	// binary's default consensus version (which is 0.3.0 after Phase 2)
	// at the right place — no override needed.
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}

	// === Deploy ===
	t.Log("deploying governance-trusted-forwarders-contract...")
	govId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     govWasm,
		Name:         "governance-trusted-forwarders",
		Description:  "TrustedForwarders governance E2E test contract",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploying governance contract: %v", err)
	}
	t.Logf("governance contract deployed: %s", govId)

	// === Set the contract's timelock to something small ===
	// 20 blocks ≈ 60s at the devnet's 3s/block. Picked to:
	//   - exceed the activate-before-timelock subtest's wall-clock
	//     budget (the subtest's 6s sleep + L1 → L2 contract output
	//     polling can chew ~30s+ on a fresh devnet), so the "real"
	//     activate that follows still finds the pending-add entry.
	//   - stay short enough that waiting it out + a buffer per
	//     criterion keeps the total test under ~12min.
	const testTimelockBlocks = 20
	if _, err := d.CallContract(ctx, 1, govId, "setTimelock", fmt.Sprintf("%d", testTimelockBlocks)); err != nil {
		t.Fatalf("setTimelock: %v", err)
	}
	t.Logf("set governance timelock to %d blocks", testTimelockBlocks)

	// === Configure sysconfig + restart magi nodes ===
	// Point magi at the freshly-deployed governance contract.
	// resolveTrustedForwarders will read its "active" state key on
	// every TxVscCallContract.ExecuteTx after this.
	if d.cfg.SysConfigOverrides == nil {
		d.cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{}
	}
	d.cfg.SysConfigOverrides.TrustedForwardersGovernanceContractId = strPtr(govId)
	if err := writeSysConfigOverrides(d.cfg, d.devnetDir); err != nil {
		t.Fatalf("writeSysConfigOverrides: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("RestartAllMagiNodes: %v", err)
	}
	// Magi nodes that were just force-recreated need additional time to
	// reattach the L1 streamer and catch up on missed blocks before they
	// start processing new contract calls. RestartAllMagiNodes only
	// sleeps 15s; in practice that's enough for the docker container
	// to start but not for the L1 backfill, so the FIRST CallContract
	// after the restart often has its L1 tx broadcast before magi is
	// ready to observe it. 30s extra is empirically enough on this
	// 4-node devnet.
	t.Log("waiting 30s for magi L1 streamers to catch up post-restart...")
	time.Sleep(30 * time.Second)

	// === Test target id we'll cycle through propose / activate /
	// remove / emergency revoke. Not a real deployed contract — the
	// magi side only treats this as a string id when reading the
	// active list, so we don't need to materialize a real forwarder
	// for the governance state-flip tests.
	const testForwarderId = "contract:vsc1Bgove2etestforwarder0001"

	// ===== Criterion 3: propose → wait → activate =====
	t.Log("=== criterion 3: propose → wait → activate ===")
	proposeTxId, err := d.CallContract(ctx, 1, govId, "proposeForwarder", testForwarderId)
	if err != nil {
		t.Fatalf("proposeForwarder: %v", err)
	}
	t.Logf("proposeForwarder L1 tx=%s", proposeTxId)
	if err := waitForContractOutput(ctx, t, d, proposeTxId, "proposeForwarder"); err != nil {
		t.Fatal(err)
	}

	// Verify pending-add was recorded.
	pending := pollStateKey(ctx, t, d, govId, "pending-add", 20*time.Second)
	if !strings.Contains(pending, testForwarderId) {
		t.Errorf("expected pending-add to contain %q, got %q", testForwarderId, pending)
	}
	t.Logf("pending-add observed: %s", pending)

	// Try to activate BEFORE the timelock expires. Should fail with
	// ErrTimelock. We use t.Run for clarity.
	t.Run("activate-before-timelock-fails", func(t *testing.T) {
		earlyTx, _ := d.CallContract(ctx, 1, govId, "activateForwarder", testForwarderId)
		// CallContract doesn't surface the contract ABORT — we have to
		// inspect the contract output. Poll briefly for the result.
		time.Sleep(6 * time.Second)
		outs, qerr := d.FindContractOutputByInput(ctx, 1, earlyTx)
		if qerr != nil || len(outs) == 0 {
			t.Logf("no output yet for early-activate tx=%s (qerr=%v); permitting", earlyTx, qerr)
			return
		}
		if len(outs[0].Results) == 0 {
			t.Errorf("activate-early should have produced a result row")
			return
		}
		ok := outs[0].Results[0].Ok
		errMsg := outs[0].Results[0].ErrMsg
		if ok {
			t.Errorf("activate-before-timelock should have failed, got ok=true")
		}
		if !strings.Contains(errMsg, "ErrTimelock") && !strings.Contains(errMsg, "still locked") {
			t.Errorf("expected ErrTimelock-style error, got: %q", errMsg)
		}
	})

	// Wait out the timelock + a buffer for indexing.
	waitBlocks := testTimelockBlocks + 4
	t.Logf("waiting %d blocks for timelock to expire...", waitBlocks)
	time.Sleep(time.Duration(waitBlocks) * 3 * time.Second)

	activateTxId, err := d.CallContract(ctx, 1, govId, "activateForwarder", testForwarderId)
	if err != nil {
		t.Fatalf("activateForwarder: %v", err)
	}
	t.Logf("activateForwarder L1 tx=%s", activateTxId)
	if err := waitForContractOutput(ctx, t, d, activateTxId, "activateForwarder"); err != nil {
		t.Fatal(err)
	}
	active := pollStateKey(ctx, t, d, govId, "active", 20*time.Second)
	if !strings.Contains(active, testForwarderId) {
		t.Fatalf("active did not land: got %q (expected to contain %q)", active, testForwarderId)
	}
	t.Logf("active post-activate: %s", active)

	// ===== Criterion 4: propose-remove → wait → activate-remove =====
	t.Log("=== criterion 4: propose-remove → wait → activate-remove ===")
	removeTxId, err := d.CallContract(ctx, 1, govId, "proposeRemoveForwarder", testForwarderId)
	if err != nil {
		t.Fatalf("proposeRemoveForwarder: %v", err)
	}
	t.Logf("proposeRemoveForwarder L1 tx=%s", removeTxId)
	if err := waitForContractOutput(ctx, t, d, removeTxId, "proposeRemoveForwarder"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Duration(waitBlocks) * 3 * time.Second)

	actRemoveTxId, err := d.CallContract(ctx, 1, govId, "activateRemoveForwarder", testForwarderId)
	if err != nil {
		t.Fatalf("activateRemoveForwarder: %v", err)
	}
	if err := waitForContractOutput(ctx, t, d, actRemoveTxId, "activateRemoveForwarder"); err != nil {
		t.Fatal(err)
	}
	// Poll until active key no longer contains the id. Empty / missing
	// is the success case; pollStateKey returns "" on miss which is
	// fine (the id can't be a substring of "").
	deadline := time.Now().Add(20 * time.Second)
	postRemoveActive := ""
	for time.Now().Before(deadline) {
		st, _ := d.GetStateByKeys(ctx, 1, govId, []string{"active"})
		if v, _ := st["active"].(string); !strings.Contains(v, testForwarderId) {
			postRemoveActive = v
			break
		}
		time.Sleep(2 * time.Second)
	}
	if strings.Contains(postRemoveActive, testForwarderId) {
		t.Errorf("expected %q to be removed; active=%q", testForwarderId, postRemoveActive)
	}
	t.Logf("active post-remove: %q", postRemoveActive)

	// ===== Criterion 5: emergencyRevoke =====
	t.Log("=== criterion 5: emergencyRevoke (immediate, no timelock) ===")
	rePropTxId, err := d.CallContract(ctx, 1, govId, "proposeForwarder", testForwarderId)
	if err != nil {
		t.Fatalf("re-proposeForwarder: %v", err)
	}
	if err := waitForContractOutput(ctx, t, d, rePropTxId, "re-proposeForwarder"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Duration(waitBlocks) * 3 * time.Second)

	reActTxId, err := d.CallContract(ctx, 1, govId, "activateForwarder", testForwarderId)
	if err != nil {
		t.Fatalf("re-activateForwarder: %v", err)
	}
	if err := waitForContractOutput(ctx, t, d, reActTxId, "re-activateForwarder"); err != nil {
		t.Fatal(err)
	}
	reActActive := pollStateKey(ctx, t, d, govId, "active", 20*time.Second)
	if !strings.Contains(reActActive, testForwarderId) {
		t.Fatalf("re-activate didn't land; active=%q", reActActive)
	}

	emTxId, err := d.CallContract(ctx, 1, govId, "emergencyRevoke", testForwarderId)
	if err != nil {
		t.Fatalf("emergencyRevoke: %v", err)
	}
	if err := waitForContractOutput(ctx, t, d, emTxId, "emergencyRevoke"); err != nil {
		t.Fatal(err)
	}
	// Poll until id is absent.
	deadline = time.Now().Add(20 * time.Second)
	postEmergency := ""
	for time.Now().Before(deadline) {
		st, _ := d.GetStateByKeys(ctx, 1, govId, []string{"active"})
		if v, _ := st["active"].(string); !strings.Contains(v, testForwarderId) {
			postEmergency = v
			break
		}
		time.Sleep(2 * time.Second)
	}
	if strings.Contains(postEmergency, testForwarderId) {
		t.Errorf("emergencyRevoke didn't clear %q from active=%q", testForwarderId, postEmergency)
	}
	t.Logf("active post-emergencyRevoke: %q", postEmergency)

	// ===== Criterion 6: setEmergencyRevokeAllowed flip blocks revoke =====
	t.Log("=== criterion 6: setEmergencyRevokeAllowed=0 blocks emergencyRevoke ===")
	c6PropTxId, err := d.CallContract(ctx, 1, govId, "proposeForwarder", testForwarderId)
	if err != nil {
		t.Fatalf("re-proposeForwarder (criterion 6): %v", err)
	}
	if err := waitForContractOutput(ctx, t, d, c6PropTxId, "re-proposeForwarder (c6)"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Duration(waitBlocks) * 3 * time.Second)

	c6ActTxId, err := d.CallContract(ctx, 1, govId, "activateForwarder", testForwarderId)
	if err != nil {
		t.Fatalf("re-activateForwarder (criterion 6): %v", err)
	}
	if err := waitForContractOutput(ctx, t, d, c6ActTxId, "re-activateForwarder (c6)"); err != nil {
		t.Fatal(err)
	}

	disableTxId, err := d.CallContract(ctx, 1, govId, "setEmergencyRevokeAllowed", "0")
	if err != nil {
		t.Fatalf("setEmergencyRevokeAllowed=0: %v", err)
	}
	if err := waitForContractOutput(ctx, t, d, disableTxId, "setEmergencyRevokeAllowed=0"); err != nil {
		t.Fatal(err)
	}
	// Diagnostic: verify the flag actually persisted. The contract's
	// EmergencyRevokeAllowedKey is "emergency-allowed"; setter writes
	// "0" for false. If this returns anything else, the next
	// emergencyRevoke would (incorrectly) succeed because the contract
	// reads the absent / non-"0" key as true.
	flagState := pollStateKey(ctx, t, d, govId, "emergency-allowed", 20*time.Second)
	t.Logf("emergency-allowed flag after setter: %q (expected %q)", flagState, "0")
	if flagState != "0" {
		t.Errorf("emergency-allowed flag did not persist as %q; got %q", "0", flagState)
	}

	blockedTxId, _ := d.CallContract(ctx, 1, govId, "emergencyRevoke", testForwarderId)
	t.Logf("blocked emergencyRevoke L1 tx=%s", blockedTxId)
	deadline = time.Now().Add(60 * time.Second)
	blockedSeen := false
	for time.Now().Before(deadline) {
		outs, qerr := d.FindContractOutputByInput(ctx, 1, blockedTxId)
		if qerr == nil && len(outs) > 0 {
			// Dump the full output record so we can spot cross-tx
			// contamination (Inputs array referencing the wrong txid,
			// Results array misaligned with Inputs).
			for i, o := range outs {
				t.Logf("blocked-revoke output[%d] id=%s block=%d inputs=%v",
					i, o.Id, o.BlockHeight, o.Inputs)
				for j, r := range o.Results {
					inputAttribution := "<no input>"
					if j < len(o.Inputs) {
						inputAttribution = o.Inputs[j]
					}
					t.Logf("  result[%d] input=%s ok=%v ret=%q err=%q errMsg=%q",
						j, inputAttribution, r.Ok, r.Ret, r.Err, r.ErrMsg)
				}
			}
			// Find the result row that corresponds to OUR txid (defensive
			// — usually result[0] but if outputs are batched, the result
			// for our input may be at a different index).
			outRec := outs[0]
			var ourResult *ContractCallResult
			for j, inp := range outRec.Inputs {
				if inp == blockedTxId && j < len(outRec.Results) {
					r := outRec.Results[j]
					ourResult = &r
					break
				}
			}
			if ourResult == nil && len(outRec.Results) > 0 {
				// Fallback: trust positional [0] only if Inputs missing.
				t.Logf("blocked-revoke: txid %s not in output Inputs; using result[0]", blockedTxId)
				r := outRec.Results[0]
				ourResult = &r
			}
			if ourResult == nil {
				continue
			}
			blockedSeen = true
			if ourResult.Ok {
				t.Errorf("emergencyRevoke should have failed (disabled by governance), got ok=true (ret=%q errMsg=%q)",
					ourResult.Ret, ourResult.ErrMsg)
			}
			if !strings.Contains(ourResult.ErrMsg, "disabled by governance") {
				t.Errorf("expected 'disabled by governance' in errMsg, got: %q", ourResult.ErrMsg)
			}
			t.Logf("emergencyRevoke blocked as expected: %s", ourResult.ErrMsg)
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !blockedSeen {
		t.Errorf("blocked emergencyRevoke output never landed for tx=%s within 60s", blockedTxId)
	}

	// Verify the forwarder is STILL active (revoke was blocked).
	stillActive := pollStateKey(ctx, t, d, govId, "active", 10*time.Second)
	if !strings.Contains(stillActive, testForwarderId) {
		t.Errorf("forwarder should still be active after blocked revoke; active=%q", stillActive)
	}

	t.Log("=== TestTrustedForwardersGovernanceE2E: all criteria verified ===")
}

func strPtr(s string) *string { return &s }

// waitForContractOutput polls for the contract-output record of an L1
// tx until one appears (proving the call landed on the contract) or 30s
// elapses. The "invalid cid: cid too short" error from getStateByKeys
// when the contract has zero committed state goes away once any state-
// mutating call's output has been indexed; gating on the FIRST output
// is the cheapest universal precondition.
func waitForContractOutput(ctx context.Context, t *testing.T, d *Devnet, txId, label string) error {
	t.Helper()
	const timeout = 60 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		outs, err := d.FindContractOutputByInput(ctx, 1, txId)
		if err == nil && len(outs) > 0 && len(outs[0].Results) > 0 {
			r := outs[0].Results[0]
			if !r.Ok {
				return fmt.Errorf("%s aborted: err=%q errMsg=%q", label, r.Err, r.ErrMsg)
			}
			t.Logf("%s contract-output landed: ret=%q", label, r.Ret)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("%s contract-output never landed for tx=%s within %s", label, txId, timeout)
}

// pollStateKey polls a single state key until it returns a non-empty
// string or `timeout` elapses. Logs the final value (even if empty)
// for diagnostics. Returns "" on timeout or any other failure mode.
func pollStateKey(ctx context.Context, t *testing.T, d *Devnet, contractId, key string, timeout time.Duration) string {
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
