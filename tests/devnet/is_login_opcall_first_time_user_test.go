package devnet

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"
)

// TestIsLoginOpCall_FirstTimeUser_InsufficientRC exercises the
// production-realistic "first-time op=call user" path that
// TestIsLoginOpCallSmoke bypasses via the regtest-only
// seedInternalHbd enabler.
//
// In production, a fresh DashDID has zero internal HBD on the mapping
// contract. mapping.dispatchForward (spec §5.2.6) debits a small
// RC-reimbursement (~500 milli-HBD) from the SENDER's internal HBD
// before invoking the forwarder. With no prior HBD, the pre-check
// fails → forwarder dispatch is NOT invoked → target contract is
// NOT written.
//
// What this test proves:
//   - mapInstantSendV2 still wraps the user's DASH into internal
//     balance (a-dash-<did>) — the user's funds are NOT stranded.
//   - forwardQueue[fq-<txid>] surfaces a FORWARD_FAILED_INSUFFICIENT_RC
//     marker the indexer / wallet can show the user.
//   - Target contract state was NOT mutated.
//
// What it does NOT do:
//   - Assert any specific recovery path. Once the user gets HBD
//     into their internal balance (via a separate DASH→HBD swap,
//     which the smoke test seeds artificially), they can retry; that
//     retry path is its own future test.
//
// Why this matters for operator-time: the failure mode is silent on
// the IS-service side (L2 submission still succeeds because
// mapInstantSendV2 itself returns ok=true). Without a test, the
// regression "dispatchForward's RC pre-check stops aborting
// gracefully" would only surface when a real user hit it on testnet —
// and the user-visible symptom (a Dash payment that DIDN'T trigger
// the expected target call) is exactly the kind of "did my deposit
// land?" support ticket that costs operator hours.
//
// Run: go test -v -run TestIsLoginOpCall_FirstTimeUser_InsufficientRC -timeout 20m ./tests/devnet/
func TestIsLoginOpCall_FirstTimeUser_InsufficientRC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping first-time-user op=call devnet test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()

	mappingWasm, err := DashMappingContractPath()
	if err != nil {
		t.Fatalf("%v", err)
	}
	forwarderWasm, err := DashForwarderContractPath()
	if err != nil {
		t.Fatalf("%v", err)
	}
	callTssWasm, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("BuildCallTssContract: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	cfg.LogLevel = "info,is-service=debug"
	cfg.EnableDashd = true
	cfg.MagiEnv = map[string]string{
		"MAGI_ISLOCK_ENABLE":          "true",
		"MAGI_ISLOCK_DASHD_RPC":       "http://dashd:19898",
		"MAGI_ISLOCK_DASHD_USER":      "vsc-node-user",
		"MAGI_ISLOCK_DASHD_PASS":      "vsc-node-pass",
		"MAGI_ISLOCK_ACCEPT_UNLOCKED": "true",
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
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

	if _, err := d.MineDashBlocks(ctx, 110); err != nil {
		t.Fatalf("mining initial dash blocks: %v", err)
	}
	seedHeader, err := d.GetDashBlockHeaderHex(ctx, 1)
	if err != nil {
		t.Fatalf("reading seed header: %v", err)
	}

	mappingId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     mappingWasm,
		Name:         "dash-mapping-contract",
		Description:  "first-time-user op=call test mapping",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy mapping: %v", err)
	}
	forwarderId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     forwarderWasm,
		Name:         "dash-forwarder-contract",
		Description:  "first-time-user op=call test forwarder",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy forwarder: %v", err)
	}
	targetId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "call-tss-target",
		Description:  "first-time-user op=call test target",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy call-tss: %v", err)
	}
	t.Logf("mapping=%s forwarder=%s target=%s", mappingId, forwarderId, targetId)

	// Wire mapping ↔ forwarder.
	if _, err := d.CallContract(ctx, 1, mappingId, "setForwarderContractId", forwarderId); err != nil {
		t.Fatalf("setForwarderContractId: %v", err)
	}
	if err := d.SetDashMappingContractId(mappingId); err != nil {
		t.Fatalf("SetDashMappingContractId: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("RestartAllMagiNodes: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, forwarderId, "init", mappingId); err != nil {
		t.Fatalf("forwarder.init: %v", err)
	}

	primaryPub, backupPub, l2Priv, err := GenerateIsTestKeys()
	if err != nil {
		t.Fatalf("GenerateIsTestKeys: %v", err)
	}
	l2DID, err := ComputeL2DIDFromPrivHex(l2Priv)
	if err != nil {
		t.Fatalf("ComputeL2DIDFromPrivHex: %v", err)
	}

	seedPayload := fmt.Sprintf(`{"block_header":"%s","block_height":1}`, seedHeader)
	if _, err := d.CallContract(ctx, 1, mappingId, "seedBlocks", seedPayload); err != nil {
		t.Fatalf("seedBlocks: %v", err)
	}
	regPayload := fmt.Sprintf(`{"primary_public_key":%q,"backup_public_key":%q}`, primaryPub, backupPub)
	if _, err := d.CallContract(ctx, 1, mappingId, "registerPublicKey", regPayload); err != nil {
		t.Fatalf("registerPublicKey: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, mappingId, "setAllowedTargetImmediate", targetId); err != nil {
		t.Fatalf("setAllowedTargetImmediate: %v", err)
	}
	validatorPayload, err := d.BuildValidatorSetPayload(0, []int{1})
	if err != nil {
		t.Fatalf("BuildValidatorSetPayload: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, mappingId, "setValidatorSet", validatorPayload); err != nil {
		t.Fatalf("setValidatorSet: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, mappingId, "setMinAttestations", "1"); err != nil {
		t.Fatalf("setMinAttestations: %v", err)
	}

	if err := d.FundL2Account(ctx, l2DID, "100.000", 12*time.Second); err != nil {
		t.Fatalf("FundL2Account(IS service): %v", err)
	}
	// IS-service submitter still needs HBD on the mapping contract's L2
	// account to settle the rcReimbursementHBD transfer that runs AFTER
	// mapInstantSendV2's main work (mapinstantsend_v2.go:334). Funding
	// this is unrelated to the *sender's* internal HBD — which is what
	// dispatchForward debits, and which the smoke test pre-seeds. This
	// test deliberately leaves the sender's internal HBD at zero.
	if err := d.FundL2Account(ctx, "contract:"+mappingId, "100.000", 12*time.Second); err != nil {
		t.Fatalf("FundL2Account(contract:%s): %v", mappingId, err)
	}

	// Wait for vs-0 indexing — same flake-avoidance pattern as the smoke test.
	const vsKey = "vs-0"
	vsDeadline := time.Now().Add(60 * time.Second)
	vsIndexed := false
	for time.Now().Before(vsDeadline) {
		st, qerr := d.GetStateByKeys(ctx, 1, mappingId, []string{vsKey})
		if qerr == nil && st != nil {
			if v, ok := st[vsKey].(string); ok && v != "" {
				vsIndexed = true
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled waiting for vs-0")
		case <-time.After(2 * time.Second):
		}
	}
	if !vsIndexed {
		t.Fatalf("vs-0 not indexed within 60s — IS service would cache empty validator set")
	}

	magi1Addr, err := d.MagiPeerMultiaddr(1)
	if err != nil {
		t.Fatalf("MagiPeerMultiaddr: %v", err)
	}
	isOpts := IsServiceOpts{
		PrimaryPubkey:         primaryPub,
		BackupPubkey:          backupPub,
		L2GqlURL:              "http://magi-1:8080/api/v1/graphql",
		L2PrivKeyHex:          l2Priv,
		L2DashContract:        mappingId,
		AddressSignerSecret:   "fresh-user-hmac-dev-only",
		BootstrapPeers:        magi1Addr,
		TestBypassDashdISLock: true,
	}
	if err := d.StartIsService(ctx, isOpts); err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs (startup fail):\n%s", isLogs)
		}
		dumpLogs(t, d, ctx)
		t.Fatalf("starting is-service: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = d.StopIsService(stopCtx)
	})

	t.Log("waiting 30s for gossipsub mesh to form...")
	select {
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		t.Fatal("ctx cancelled waiting for mesh")
	}

	const targetKey = "fresh-user-key"
	const targetVal = "fresh-user-val"
	argsB64 := base64.StdEncoding.EncodeToString([]byte(targetKey + "," + targetVal))
	resp, err := d.IsStartSession(ctx, IsSessionStartReq{
		Op: "call",
		Args: map[string]interface{}{
			"contract": targetId,
			"method":   "setString",
			"args":     argsB64,
			"amount":   0,
		},
	})
	if err != nil {
		t.Fatalf("/session/start: %v", err)
	}
	t.Logf("op=call session: sid=%s deposit=%s", resp.Sid, resp.DepositAddress)

	t.Logf("paying real Dash to %s...", resp.DepositAddress)
	dashTxId, err := d.SendDashTo(ctx, resp.DepositAddress, "0.001")
	if err != nil {
		t.Fatalf("SendDashTo: %v", err)
	}
	t.Logf("dashd tx: %s", dashTxId)

	// Resolve the sender DashDID for later assertions but DO NOT
	// call seedInternalHbd — that's the entire point.
	senderAddr, err := d.ResolveDashTxSenderAddress(ctx, dashTxId)
	if err != nil {
		t.Fatalf("resolving sender of dash tx %s: %v", dashTxId, err)
	}
	senderDID := DashTestnetDIDFromAddress(senderAddr)
	t.Logf("sender DashDID (NOT pre-seeded with HBD): %s", senderDID)

	if _, err := d.WaitForIsSessionState(ctx, resp.Sid,
		[]string{"ATTESTING", "L2_SUBMITTED", "ON_CHAIN"},
		45*time.Second); err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs (waiting for ATTESTING):\n%s", isLogs)
		}
		t.Fatalf("session did not reach ATTESTING: %v", err)
	}

	rawTxHex, err := d.GetDashRawTransaction(ctx, dashTxId)
	if err != nil {
		t.Fatalf("GetDashRawTransaction: %v", err)
	}
	instructionBytes, err := hex.DecodeString(resp.DepositInstructionHex)
	if err != nil {
		t.Fatalf("decoding DepositInstructionHex: %v", err)
	}
	if err := d.IsForceAttestation(ctx, resp.Sid, dashTxId, rawTxHex, string(instructionBytes), 1); err != nil {
		t.Fatalf("IsForceAttestation: %v", err)
	}
	t.Logf("injected magi-1 attestation for sid=%s", resp.Sid)

	final, err := d.WaitForIsSessionState(ctx, resp.Sid,
		[]string{"L2_SUBMITTED", "ON_CHAIN"},
		120*time.Second)
	if err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs (final-state wait):\n%s", isLogs)
		}
		t.Fatalf("session did not reach L2_SUBMITTED/ON_CHAIN: %v", err)
	}
	t.Logf("L2 submitted: state=%s l2TxId=%s", final.State, final.L2TxId)
	if final.L2TxId == "" {
		t.Fatal("expected non-empty l2TxId after L2 submission")
	}

	// === Assertions =====================================================
	//
	// 1. Target contract state was NOT written — the forwarder dispatch
	//    didn't run because the HBD pre-check failed.
	// 2. forwardQueue["fq-<internalTxid>"] surfaces a failure marker.
	// 3. The user's wrapped DASH is credited to a-dash-<did> — funds
	//    are recoverable.
	//
	// internalTxid: contract's `hex(sha256d(rawTxBytes))` = dashTxId
	// byte-reversed (dashd displays the reversed form by convention).
	internalTxid := reverseHexBytes(dashTxId)
	fqKey := "fq-" + internalTxid
	// DASH balance lives at "a-<did>" (the bare BalancePrefix + vscAcc
	// path used by setAccBal/getAccBal in mapping/utils.go) — NOT
	// "a-dash-<did>" like other assets. The "a-<asset>-<did>" form is
	// only for non-native assets (e.g. HBD). See
	// forwarder_integration.go:483 getInternalBalance for the branch
	// that re-routes asset=="dash" through getAccBal.
	dashBalKey := "a-" + senderDID
	hbdBalKey := "a-hbd-" + senderDID

	// (1) Target write should be absent. Poll briefly to make sure the
	// failure isn't just a slow indexer.
	const negativeTargetPoll = 30 * time.Second
	targetDeadline := time.Now().Add(negativeTargetPoll)
	for time.Now().Before(targetDeadline) {
		st, _ := d.GetStateByKeys(ctx, 1, targetId, []string{targetKey})
		if st != nil {
			if v, _ := st[targetKey].(string); v == targetVal {
				t.Fatalf("target[%q]=%q — forwarder dispatch ran despite an empty sender HBD balance; the RC pre-check is no longer firing",
					targetKey, v)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while polling target")
		case <-time.After(3 * time.Second):
		}
	}
	t.Logf("target[%q] was not written ✓ (forwarder dispatch correctly skipped)", targetKey)

	// (2) forwardQueue should record the failure. The marker shape is
	// the mapping contract's StatusForwardFailedInsufficientRC. The
	// state value is contract-internal bytes; a non-empty hex readback
	// is sufficient — we don't decode the marker enum, just check
	// SOMETHING is there. If forwardQueue is empty entirely, the
	// dispatcher never enqueued, which is a separate bug.
	fqDeadline := time.Now().Add(60 * time.Second)
	fqSeen := false
	for time.Now().Before(fqDeadline) {
		st, qerr := d.GetStateByKeysHex(ctx, 1, mappingId, []string{fqKey})
		if qerr == nil && st != nil {
			if v, ok := st[fqKey].(string); ok && v != "" {
				t.Logf("forwardQueue[%s]=%s (hex) — dispatcher recorded the failure", fqKey, v)
				fqSeen = true
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while polling forwardQueue")
		case <-time.After(3 * time.Second):
		}
	}
	if !fqSeen {
		t.Errorf("forwardQueue[%s] is empty — dispatcher did not enqueue the failed forward (expected a FORWARD_FAILED_INSUFFICIENT_RC marker)",
			fqKey)
	}

	// (3) Sender's wrapped DASH should be credited (deposit amount,
	// minus any base-rate fee). The wrap happens BEFORE
	// dispatchForward, so failure of dispatch doesn't roll back the
	// wrap. A non-zero balance is the correctness signal.
	dashBalSeen := false
	dashDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(dashDeadline) {
		bal, qerr := d.GetStateByKeysHex(ctx, 1, mappingId, []string{dashBalKey})
		if qerr == nil && bal != nil {
			if v, ok := bal[dashBalKey].(string); ok && v != "" && !isAllZeros(v) {
				t.Logf("wrapped DASH credited: %s=%s (hex) — funds are recoverable", dashBalKey, v)
				dashBalSeen = true
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while polling wrapped DASH balance")
		case <-time.After(3 * time.Second):
		}
	}
	if !dashBalSeen {
		t.Errorf("wrapped DASH balance %s is empty — user's deposit was lost (this is a regression: the wrap must succeed even when dispatchForward fails)",
			dashBalKey)
	}

	// Diagnostic: hbd balance should be zero (or absent). We don't fail
	// on this — depending on how indexer represents zero/empty, the key
	// may simply not exist. Log for context.
	if hbdBal, qerr := d.GetStateByKeysHex(ctx, 1, mappingId, []string{hbdBalKey}); qerr == nil {
		t.Logf("sender HBD balance %s=%v (expected empty/zero)", hbdBalKey, hbdBal[hbdBalKey])
	}

	// Bonus diagnostic: pull the mapInstantSendV2 contract-output to see
	// what status it returned. The ok=true return + visible
	// FORWARD_FAILED_INSUFFICIENT_RC is the production-observable signal.
	if final.L2TxId != "" {
		if outs, qerr := d.FindContractOutputByInput(ctx, 1, final.L2TxId); qerr == nil {
			for _, o := range outs {
				for i, r := range o.Results {
					input := "<no input>"
					if i < len(o.Inputs) {
						input = o.Inputs[i]
					}
					t.Logf("misv-batch call[%d] input=%s ok=%v ret=%q err=%q errMsg=%q",
						i, input, r.Ok, r.Ret, r.Err, r.ErrMsg)
					if r.Ok && strings.Contains(r.Ret, "INSUFFICIENT_RC") {
						t.Logf("✓ mapInstantSendV2 ret surfaces INSUFFICIENT_RC marker as expected")
					}
				}
			}
		}
	}

	t.Log("=== TestIsLoginOpCall_FirstTimeUser_InsufficientRC: PASS — fresh DashDID's dispatchForward fails gracefully, deposit is recoverable as wrapped DASH ===")
}

// isAllZeros returns true if the hex string is empty or every nibble
// is '0'. Used to distinguish a "key exists but value is zero" balance
// (treated as no funds) from a "key missing entirely" balance.
func isAllZeros(hexStr string) bool {
	if hexStr == "" {
		return true
	}
	for i := 0; i < len(hexStr); i++ {
		if hexStr[i] != '0' {
			return false
		}
	}
	return true
}
