package devnet

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"
)

// TestIsLoginOpCall_TwoSequential runs the op=call IS-login pipeline
// TWICE in a row against the same devnet + same target contract +
// same sender DashDID. Proves the parts that are inherently invisible
// to single-call coverage:
//
//   - Idempotency of the wiring (forwarder allow-list, validator-set
//     cache, gossipsub mesh, IS-service per-session state) across
//     back-to-back sessions.
//   - State accumulation: the second call's write must land WITHOUT
//     overwriting the first call's write (different keys).
//   - Per-call seedInternalHbd accounting: a single pre-seed must
//     cover BOTH dispatches' RC-reimbursement debits.
//   - forwardQueue + contract-output history correctly attributes
//     two distinct Dash txids to two distinct dispatches.
//
// Operator-time-saved: if the dispatcher accidentally caches
// per-sender allowance state in a way that leaks across calls (a
// classic stale-state regression), this test catches it long before
// a real user notices their second deposit silently failing.
//
// Run: go test -v -run TestIsLoginOpCall_TwoSequential -timeout 25m ./tests/devnet/
func TestIsLoginOpCall_TwoSequential(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sequential op=call devnet test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Minute)
	defer cancel()

	// G requires seedInternalHbd + setAllowedTargetImmediate, both
	// regtest-only in the dash-mapping-contract. The default
	// testnet.wasm rejects them; only dev.wasm (NetworkMode=regtest)
	// accepts. resolveDashWorkWasm (dash_work_wasm.go) picks dev.wasm
	// from the feature-build clone in priority order:
	// env var > dash_work/utxo-mapping/.../dev.wasm > default helper.
	mappingWasm := resolveDashWorkWasm("DASH_MAPPING_WASM_PATH",
		"dash-mapping-contract", DashMappingContractPath)
	forwarderWasm := resolveDashWorkWasm("DASH_FORWARDER_WASM_PATH",
		"dash-forwarder-contract", DashForwarderContractPath)
	if mappingWasm == "" {
		t.Fatal("dash-mapping wasm could not be resolved")
	}
	if forwarderWasm == "" {
		t.Fatal("dash-forwarder wasm could not be resolved")
	}
	t.Logf("using dash-mapping wasm: %s", mappingWasm)
	t.Logf("using dash-forwarder wasm: %s", forwarderWasm)
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
		t.Fatalf("MineDashBlocks: %v", err)
	}
	seedHeader, err := d.GetDashBlockHeaderHex(ctx, 1)
	if err != nil {
		t.Fatalf("GetDashBlockHeaderHex: %v", err)
	}

	mappingId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: mappingWasm, Name: "dash-mapping-contract",
		Description: "sequential op=call test mapping", DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy mapping: %v", err)
	}
	forwarderId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: forwarderWasm, Name: "dash-forwarder-contract",
		Description: "sequential op=call test forwarder", DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy forwarder: %v", err)
	}
	targetId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: callTssWasm, Name: "call-tss-target",
		Description: "sequential op=call test target", DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploy target: %v", err)
	}
	t.Logf("mapping=%s forwarder=%s target=%s", mappingId, forwarderId, targetId)

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
	if err := d.FundL2Account(ctx, "contract:"+mappingId, "100.000", 12*time.Second); err != nil {
		t.Fatalf("FundL2Account(contract:%s): %v", mappingId, err)
	}

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
		t.Fatalf("vs-0 not indexed within 60s")
	}

	magi1Addr, err := d.MagiPeerMultiaddr(1)
	if err != nil {
		t.Fatalf("MagiPeerMultiaddr: %v", err)
	}
	isOpts := IsServiceOpts{
		PrimaryPubkey: primaryPub, BackupPubkey: backupPub,
		L2GqlURL: "http://magi-1:8080/api/v1/graphql", L2PrivKeyHex: l2Priv,
		L2DashContract:      mappingId,
		AddressSignerSecret: "sequential-hmac-dev-only",
		BootstrapPeers:      magi1Addr, TestBypassDashdISLock: true,
	}
	if err := d.StartIsService(ctx, isOpts); err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service startup fail logs:\n%s", isLogs)
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

	// === Pre-warmup: lock in the wallet's sender address and seed the
	// internal HBD BEFORE the first round's payment fires. This
	// avoids the race where the per-round in-line seedInternalHbd
	// broadcast hadn't indexed by the time the IS service's
	// mapInstantSendV2 → dispatchForward reads HBD balance, causing
	// FORWARD_FAILED_INSUFFICIENT_RC.
	//
	// dashd regtest's sendtoaddress with small amounts always uses
	// single-address inputs (per dashd.go:144 comment), and the
	// wallet's coin-selection picks the SAME address for subsequent
	// sends until that address's UTXO is fully spent. The warmup
	// fires one tiny send to a freshly-minted address (throwaway),
	// then ResolveDashTxSenderAddress returns the wallet's funded
	// address. We seed THAT did, wait for L2 indexing, then run
	// both real rounds against the same sender.
	warmupAddr, err := d.dashCli(ctx, "getnewaddress")
	if err != nil {
		t.Fatalf("getnewaddress (warmup): %v", err)
	}
	warmupTxId, err := d.SendDashTo(ctx, warmupAddr, "0.00001")
	if err != nil {
		t.Fatalf("warmup SendDashTo: %v", err)
	}
	warmupSenderAddr, err := d.ResolveDashTxSenderAddress(ctx, warmupTxId)
	if err != nil {
		t.Fatalf("resolving warmup sender: %v", err)
	}
	walletDID := DashTestnetDIDFromAddress(warmupSenderAddr)
	t.Logf("pre-warmup: wallet's coin-selection sender DID = %s", walletDID)

	const seedAmountMilliHbd = 12000 // 2 rounds × ~500 mhbd debit, 12× margin
	seedHbdPayload := fmt.Sprintf("%s,%d", walletDID, seedAmountMilliHbd)
	seedTxId, err := d.CallContract(ctx, 1, mappingId, "seedInternalHbd", seedHbdPayload)
	if err != nil {
		t.Fatalf("seedInternalHbd(warmup): %v", err)
	}
	t.Logf("seedInternalHbd broadcast for %s (L1 tx=%s); polling for L2 indexing...", walletDID, seedTxId)

	// Wait until a-hbd-<did> is observable via L2 GQL. 60s budget;
	// observed indexing latency is 10-30s.
	seedKey := "a-hbd-" + walletDID
	seedDeadline := time.Now().Add(60 * time.Second)
	seedIndexed := false
	for time.Now().Before(seedDeadline) {
		bal, qerr := d.GetStateByKeysHex(ctx, 1, mappingId, []string{seedKey})
		if qerr == nil && bal != nil {
			if v, ok := bal[seedKey].(string); ok && v != "" {
				t.Logf("seed indexed: %s=%s (hex) ✓", seedKey, v)
				seedIndexed = true
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while polling seed-balance")
		case <-time.After(2 * time.Second):
		}
	}
	if !seedIndexed {
		t.Fatalf("warmup seed at %s did not index within 60s — dispatch would fail on both rounds", seedKey)
	}

	runOpCall := func(round int, targetKey, targetVal string, expectedSenderDID string) (senderDIDOut string) {
		t.Logf("=== round %d: setString(%q, %q) ===", round, targetKey, targetVal)
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
			t.Fatalf("round %d /session/start: %v", round, err)
		}
		t.Logf("round %d session: sid=%s deposit=%s", round, resp.Sid, resp.DepositAddress)

		dashTxId, err := d.SendDashTo(ctx, resp.DepositAddress, "0.001")
		if err != nil {
			t.Fatalf("round %d SendDashTo: %v", round, err)
		}
		t.Logf("round %d dashd tx: %s", round, dashTxId)

		senderAddr, err := d.ResolveDashTxSenderAddress(ctx, dashTxId)
		if err != nil {
			t.Fatalf("round %d resolving sender: %v", round, err)
		}
		senderDID := DashTestnetDIDFromAddress(senderAddr)
		senderDIDOut = senderDID
		t.Logf("round %d sender DashDID: %s", round, senderDID)

		// Sanity: the round's sender DID must match the walletDID
		// we pre-seeded. If dashd's coin-selection picks a different
		// address (e.g. UTXO churn), the seed wouldn't cover this
		// round's dispatch and the test's single-seed assumption is
		// invalid.
		if senderDID != expectedSenderDID {
			t.Fatalf("round %d sender DashDID %q does not match the pre-seeded walletDID %q — dashd wallet coin-selection switched addresses; warmup-seed strategy is invalid",
				round, senderDID, expectedSenderDID)
		}

		if _, err := d.WaitForIsSessionState(ctx, resp.Sid,
			[]string{"ATTESTING", "L2_SUBMITTED", "ON_CHAIN"},
			45*time.Second); err != nil {
			t.Fatalf("round %d did not reach ATTESTING: %v", round, err)
		}

		rawTxHex, err := d.GetDashRawTransaction(ctx, dashTxId)
		if err != nil {
			t.Fatalf("round %d GetDashRawTransaction: %v", round, err)
		}
		instructionBytes, err := hex.DecodeString(resp.DepositInstructionHex)
		if err != nil {
			t.Fatalf("round %d decoding instruction: %v", round, err)
		}
		if err := d.IsForceAttestation(ctx, resp.Sid, dashTxId, rawTxHex, string(instructionBytes), 1); err != nil {
			t.Fatalf("round %d IsForceAttestation: %v", round, err)
		}

		if _, err := d.WaitForIsSessionState(ctx, resp.Sid,
			[]string{"L2_SUBMITTED", "ON_CHAIN"},
			120*time.Second); err != nil {
			if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
				t.Logf("round %d is-service logs:\n%s", round, isLogs)
			}
			t.Fatalf("round %d did not reach L2_SUBMITTED: %v", round, err)
		}

		// Poll target until the round's write lands. 180s window —
		// 90s was tight enough to flake under op=call's heavier L2
		// indexing load. Doubled, matching the seedInternalHbd poll
		// budget the smoke test uses.
		statePollDeadline := time.Now().Add(180 * time.Second)
		var lastObserved any
		for time.Now().Before(statePollDeadline) {
			st, _ := d.GetStateByKeys(ctx, 1, targetId, []string{targetKey})
			if st != nil {
				lastObserved = st[targetKey]
				if v, ok := st[targetKey].(string); ok && v == targetVal {
					t.Logf("round %d target[%q]=%q ✓", round, targetKey, v)
					return senderDID
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal("ctx cancelled waiting for target write")
			case <-time.After(3 * time.Second):
			}
		}
		// Diagnostics on failure: mirror the smoke test's dispatch-
		// diagnostics dump so we can attribute "target never written"
		// to a specific stage in the dispatch chain.
		internalTxid := reverseHexBytes(dashTxId)
		fqKey := "fq-" + internalTxid
		atKey := "at-" + targetId
		ibKey := "a-hbd-" + senderDID
		if diag, derr := d.GetStateByKeysHex(ctx, 1, mappingId,
			[]string{fqKey, atKey, ibKey, "forwarder"}); derr == nil {
			t.Logf("round %d dispatch diagnostics (hex) — forwardQueue[%s]=%v allowedTargets[%s]=%v internalHbd[%s]=%v forwarderId=%v",
				round, fqKey, diag[fqKey], atKey, diag[atKey], ibKey, diag[ibKey], diag["forwarder"])
		} else {
			t.Logf("round %d dispatch diagnostics: GetStateByKeysHex failed: %v", round, derr)
		}
		// Pull the IS service's L2 submission's contract-output(s).
		l2TxId := ""
		if sess, serr := d.WaitForIsSessionState(ctx, resp.Sid,
			[]string{"L2_SUBMITTED", "ON_CHAIN"}, 1*time.Second); serr == nil {
			l2TxId = sess.L2TxId
		}
		if l2TxId != "" {
			if outs, qerr := d.FindContractOutputByInput(ctx, 1, l2TxId); qerr == nil {
				for _, o := range outs {
					for i, r := range o.Results {
						input := "<no input>"
						if i < len(o.Inputs) {
							input = o.Inputs[i]
						}
						t.Logf("round %d misv-batch call[%d] input=%s ok=%v ret=%q err=%q errMsg=%q",
							round, i, input, r.Ok, r.Ret, r.Err, r.ErrMsg)
					}
				}
			}
		}
		t.Fatalf("round %d target[%q] never became %q within 180s (last observed: %v)",
			round, targetKey, targetVal, lastObserved)
		return senderDID
	}

	_ = runOpCall(1, "key-round1", "val-round1", walletDID)
	_ = runOpCall(2, "key-round2", "val-round2", walletDID)

	// Boundary assertion: BOTH writes must coexist on the target.
	// Per-round runOpCall already confirmed each individual write
	// landed, but this final read proves the second call did not
	// clobber the first (e.g. a forwarder cache that overwrote
	// instead of accumulating).
	finalSt, ferr := d.GetStateByKeys(ctx, 1, targetId,
		[]string{"key-round1", "key-round2"})
	if ferr != nil {
		t.Fatalf("final target state read: %v", ferr)
	}
	if v, _ := finalSt["key-round1"].(string); v != "val-round1" {
		t.Errorf("round 1 write was overwritten / lost: target[\"key-round1\"]=%q, expected %q",
			v, "val-round1")
	}
	if v, _ := finalSt["key-round2"].(string); v != "val-round2" {
		t.Errorf("round 2 write missing: target[\"key-round2\"]=%q, expected %q",
			v, "val-round2")
	}

	t.Log("=== TestIsLoginOpCall_TwoSequential: PASS — two back-to-back op=call dispatches both landed at the target with no state clobbering ===")
}
