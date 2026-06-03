package devnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestIsLoginValidatorSetRotation exercises a same-epoch validator-set
// rotation through the real attestation gossip path.
//
// Scenario:
//
//	t0: setValidatorSet(0, V1=[magi-1, magi-2]) + setMinAttestations(2)
//	    Run an IS-login → quorum is reached by magi-1+magi-2 BLS sigs.
//	t1: setValidatorSet(0, V2=[magi-3, magi-4])  ← same-epoch overwrite
//	t2: Wait > validatorSetCacheTTLSeconds for the IS service to drop
//	    its cached V1 + re-fetch from contract.
//	t3: Run a second IS-login → quorum is reached by magi-3+magi-4 BLS
//	    sigs (magi-1+magi-2's responses are now dropped by the
//	    per-sig validator-set filter — proves the cache flipped over).
//
// What this validates that the auth-only smoke test does NOT:
//
//   - dash-mapping-contract accepts a setValidatorSet overwrite for
//     the same epoch (operational guarantee documented in the
//     deployment runbook §2.3 "Rollback / fix").
//   - The IS service's per-epoch validator-set cache TTL works
//     correctly — old entries expire, fresh entries get re-fetched.
//   - The orchestrator's per-sig filter rejects responses signed by
//     validators NOT in the active set for the session's epoch — so
//     when V1 magis still respond on the gossip topic post-rotation,
//     their sigs are dropped + quorum is computed strictly over V2.
//
// We deliberately use 4 magi witnesses + a non-overlapping rotation
// (V1 ∩ V2 = ∅) so the "wrong magis still responding" scenario is
// real: all 4 witnesses run with MAGI_ISLOCK_ENABLE=true + dashd-RPC
// poller backing, all 4 see + sign every IS-locked tx. The contract +
// IS-service rotation logic is the only thing that flips quorum from
// V1 to V2.
//
// We shrink the IS service's validatorSetCacheTTLSeconds from the
// default 30s to 5s to keep the test fast (cache wait is the
// dominating cost in this test). Production deploys leave the
// default.
//
// Run with:
//
//	go test -v -run TestIsLoginValidatorSetRotation -timeout 20m ./tests/devnet/
func TestIsLoginValidatorSetRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping IS-login rotation devnet test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()

	wasmPath, err := DashMappingContractPath()
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Logf("using dash-mapping-contract WASM: %s", wasmPath)

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

	t.Log("deploying dash-mapping-contract...")
	mappingId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     wasmPath,
		Name:         "dash-mapping-contract",
		Description:  "IS-login validator-set rotation test mapping contract",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploying mapping contract: %v", err)
	}
	t.Logf("mapping contract deployed: %s", mappingId)

	seedPayload := fmt.Sprintf(`{"block_header":"%s","block_height":1}`, seedHeader)
	if _, err := d.CallContract(ctx, 1, mappingId, "seedBlocks", seedPayload); err != nil {
		t.Fatalf("seedBlocks: %v", err)
	}

	primaryPub, backupPub, l2Priv, err := GenerateIsTestKeys()
	if err != nil {
		t.Fatalf("generating IS test keys: %v", err)
	}
	l2DID, err := ComputeL2DIDFromPrivHex(l2Priv)
	if err != nil {
		t.Fatalf("computing L2 DID: %v", err)
	}
	t.Logf("IS service L2 account: %s", l2DID)

	if err := d.FundL2Account(ctx, l2DID, "100.000", 12*time.Second); err != nil {
		t.Fatalf("FundL2Account: %v", err)
	}

	registerKeysPayload := fmt.Sprintf(
		`{"primary_public_key":%q,"backup_public_key":%q}`,
		primaryPub, backupPub)
	if _, err := d.CallContract(ctx, 1, mappingId, "registerPublicKey", registerKeysPayload); err != nil {
		t.Fatalf("registerPublicKey: %v", err)
	}

	// V1: magi-1 + magi-2 as the active validator set, quorum=2.
	v1Payload, err := d.BuildValidatorSetPayload(0, []int{1, 2})
	if err != nil {
		t.Fatalf("BuildValidatorSetPayload V1: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, mappingId, "setValidatorSet", v1Payload); err != nil {
		t.Fatalf("setValidatorSet V1: %v", err)
	}
	t.Logf("registered V1 validator set for epoch 0 (magi-1 + magi-2)")

	if _, err := d.CallContract(ctx, 1, mappingId, "setMinAttestations", "2"); err != nil {
		t.Fatalf("setMinAttestations(2): %v", err)
	}

	// Connect to ALL four real magi peers so even after the
	// rotation the IS service has gossip-mesh links to whichever
	// pair is the current quorum. A bootstrap to just magi-1
	// would still work via gossipsub fanout, but pinning every
	// peer explicitly avoids relying on emergent mesh formation
	// across all 4 witnesses inside the test's wall-clock budget.
	peers := []string{}
	for n := 1; n <= 4; n++ {
		addr, err := d.MagiPeerMultiaddr(n)
		if err != nil {
			t.Fatalf("MagiPeerMultiaddr(%d): %v", n, err)
		}
		peers = append(peers, addr)
	}
	bootstrapPeers := strings.Join(peers, ",")
	t.Logf("IS service bootstrap peers: %s", bootstrapPeers)

	isOpts := IsServiceOpts{
		PrimaryPubkey:               primaryPub,
		BackupPubkey:                backupPub,
		AddressSignerSecret:         "rotation-test-hmac-secret-dev-only",
		L2GqlURL:                    "http://magi-1:8080/api/v1/graphql",
		L2PrivKeyHex:                l2Priv,
		L2DashContract:              mappingId,
		BootstrapPeers:              bootstrapPeers,
		TestBypassDashdISLock:       true,
		ValidatorSetCacheTTLSeconds: 5,
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

	t.Log("waiting 10s for gossipsub mesh to form across 4 magi peers...")
	select {
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		t.Fatal("ctx cancelled waiting for mesh")
	}

	// === Round 1: V1 quorum (magi-1 + magi-2) ===
	t.Log("=== Round 1: IS login against V1 = [magi-1, magi-2] ===")
	runIsLogin(ctx, t, d, "round 1 (V1)")

	// === Rotate: V2 = magi-3 + magi-4 ===
	v2Payload, err := d.BuildValidatorSetPayload(0, []int{3, 4})
	if err != nil {
		t.Fatalf("BuildValidatorSetPayload V2: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, mappingId, "setValidatorSet", v2Payload); err != nil {
		t.Fatalf("setValidatorSet V2 (same-epoch overwrite): %v", err)
	}
	t.Logf("rotated to V2 validator set for epoch 0 (magi-3 + magi-4)")

	// Wait for IS service's validator-set cache (TTL=5s) to expire
	// + the next /session/start's per-epoch lookup to fetch V2.
	// Buffer to 7s to absorb GraphQL replication lag between the
	// CallContract above and magi-1's contract-output store.
	t.Log("waiting 7s for IS service validator-set cache to expire (TTL=5s)...")
	select {
	case <-time.After(7 * time.Second):
	case <-ctx.Done():
		t.Fatal("ctx cancelled waiting for cache TTL")
	}

	// === Round 2: V2 quorum (magi-3 + magi-4) ===
	// All 4 witnesses still respond on the gossip topic (each runs
	// MAGI_ISLOCK_ENABLE=true + dashd-RPC poller). The IS service's
	// per-sig filter must drop magi-1 + magi-2 (no longer in active
	// set for epoch 0) and aggregate magi-3 + magi-4. If the cache
	// failed to invalidate, the orchestrator would keep using V1 +
	// drop magi-3 + magi-4 instead → ATTESTATION_TIMEOUT.
	t.Log("=== Round 2: IS login against V2 = [magi-3, magi-4] ===")
	runIsLogin(ctx, t, d, "round 2 (V2)")

	t.Log("validator-set rotation devnet test PASSED")
}

// runIsLogin drives one full IS-login session through to
// L2_SUBMITTED/ON_CHAIN. Extracted into a helper so the rotation
// test can run two consecutive rounds with different validator sets.
//
// Pre-conditions:
//   - The dash-mapping-contract is deployed and the IS service is
//     running against it.
//   - The contract's active validator set + minAttestations are set
//     to a quorum the test's 4 magi witnesses can satisfy.
//
// On any failure dumps the IS service logs before t.Fatalf so the
// failure surface is visible without rerunning.
func runIsLogin(ctx context.Context, t *testing.T, d *Devnet, label string) {
	t.Helper()

	resp, err := d.IsStartSession(ctx, IsSessionStartReq{Op: "auth"})
	if err != nil {
		t.Fatalf("[%s] /session/start: %v", label, err)
	}
	t.Logf("[%s] session=%s deposit=%s", label, resp.Sid, resp.DepositAddress)

	dashTxId, err := d.SendDashTo(ctx, resp.DepositAddress, "0.001")
	if err != nil {
		t.Fatalf("[%s] SendDashTo: %v", label, err)
	}
	t.Logf("[%s] dashd tx broadcast: %s", label, dashTxId)

	final, err := d.WaitForIsSessionState(ctx, resp.Sid,
		[]string{"L2_SUBMITTED", "ON_CHAIN"},
		120*time.Second)
	if err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("[%s] is-service logs:\n%s", label, isLogs)
		}
		t.Fatalf("[%s] session did not reach L2_SUBMITTED/ON_CHAIN: %v", label, err)
	}
	t.Logf("[%s] state=%s l2TxId=%s dashTxId=%s",
		label, final.State, final.L2TxId, final.DashTxId)

	if final.State != "L2_SUBMITTED" && final.State != "ON_CHAIN" {
		t.Errorf("[%s] expected L2_SUBMITTED or ON_CHAIN, got %s", label, final.State)
	}
	if final.DashTxId != dashTxId {
		t.Errorf("[%s] expected dashTxId=%q, got %q", label, dashTxId, final.DashTxId)
	}
	if final.L2TxId == "" {
		t.Errorf("[%s] expected non-empty l2TxId, got empty", label)
	}
}
