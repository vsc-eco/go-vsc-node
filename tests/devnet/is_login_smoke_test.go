package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestIsLoginSmoke is the minimal devnet smoke test for the Dash
// InstantSend login feature. Goal: prove the moving parts assemble
// end-to-end without exercising the IS-lock-driven payment path
// (which requires either masternode quorums or an upstream
// IS-service test-mode that this branch doesn't add).
//
// What it verifies:
//   - dash-mapping-contract deploys cleanly to the devnet (this used
//     to flake; see commit 5f97e392 + the dash_is_login_audit_loop
//     memory note for the contract-deploy-1 / TBD-currency fix).
//   - seedBlocks lands at the contract owner (magi.test1) via
//     CallContract.
//   - is-service container starts up against the devnet — primary
//     + backup pubkeys parse, the L2 GQL endpoint is reachable,
//     dashd RPC startup probe succeeds, libp2p+pubsub-noop wiring
//     boots without error.
//   - /session/start over HTTP returns a well-shaped response
//     (sid, deposit_address, address_signature, instruction).
//
// What it does NOT verify (out of scope, future work):
//   - dashd → IS_OBSERVED transition (real IS-locks need a Dash
//     LLMQ quorum; regtest dashd doesn't produce them).
//   - Validator BLS attestation gossip (would need
//     setValidatorSet + per-magi-node BLS PoP wiring).
//   - mapInstantSendV2 L2 submission landing on the contract.
//   - End-to-end state assertions on forwardQueue entries.
//
// Marked as a smoke test in the comment so future contributors don't
// expect full-IS-login regression coverage here. The real E2E test
// is tracked under "Docker multi-node IS-login devnet test" in
// MEMORY/dash_is_login_audit_loop.
//
// Run with:
//
//	go test -v -run TestIsLoginSmoke -timeout 15m ./tests/devnet/
func TestIsLoginSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping IS-login devnet smoke test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
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

	// Mine a seed block so seedBlocks has something to chain onto.
	if _, err := d.MineDashBlocks(ctx, 5); err != nil {
		t.Fatalf("mining initial dash blocks: %v", err)
	}
	seedHeader, err := d.GetDashBlockHeaderHex(ctx, 1)
	if err != nil {
		t.Fatalf("reading seed header: %v", err)
	}

	// 1. Deploy mapping contract. Uses commit 5f97e392's
	//    vsc-deployer-1 + TBD-currency fix — should succeed on
	//    the first attempt with no retries.
	t.Log("deploying dash-mapping-contract...")
	mappingId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     wasmPath,
		Name:         "dash-mapping-contract",
		Description:  "IS-login smoke test mapping contract",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploying mapping contract: %v", err)
	}
	t.Logf("mapping contract deployed: %s", mappingId)

	// 2. seedBlocks via magi.test1 (the on-chain contract.owner).
	seedPayload := fmt.Sprintf(`{"block_header":"%s","block_height":1}`, seedHeader)
	if _, err := d.CallContract(ctx, 1, mappingId, "seedBlocks", seedPayload); err != nil {
		t.Fatalf("seedBlocks: %v", err)
	}

	// 3. Generate dummy keys for the IS service. The smoke path
	//    never exercises spending against the deposit address or
	//    L2 submission, so the keys don't need to own anything.
	primaryPub, backupPub, l2Priv, err := GenerateIsTestKeys()
	if err != nil {
		t.Fatalf("generating IS test keys: %v", err)
	}

	// 4. Start the IS service. dashd was started by EnableDashd=true
	//    so the RPC startup probe will succeed.
	isOpts := IsServiceOpts{
		PrimaryPubkey:       primaryPub,
		BackupPubkey:        backupPub,
		L2GqlURL:            "http://magi-1:8080/api/v1/graphql",
		L2PrivKeyHex:        l2Priv,
		L2DashContract:      mappingId,
		AddressSignerSecret: "smoke-test-hmac-secret-dev-only-do-not-ship",
	}
	if err := d.StartIsService(ctx, isOpts); err != nil {
		// Pull is-service's own logs first so the actual binary
		// startup error surfaces — dumpLogs() only covers
		// magi-1..N + haf. The profile-aware IsServiceLogs helper
		// runs `docker compose logs` with --profile is-service so
		// the compose CLI recognises the service.
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs:\n%s", isLogs)
		} else {
			t.Logf("could not read is-service logs: %v", lerr)
		}
		dumpLogs(t, d, ctx)
		t.Fatalf("starting is-service: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = d.StopIsService(stopCtx)
	})

	// 5. POST /session/start for an op=auth login. Verify the
	//    response carries the fields the Altera client consumes
	//    (sid + depositAddress + addressSignature + expiresAt +
	//    statusUrl). The /session/start response shape is defined
	//    in cmd/is-service/handlers.go:SessionStartResponse.
	resp, err := d.IsStartSession(ctx, IsSessionStartReq{Op: "auth"})
	if err != nil {
		t.Fatalf("/session/start: %v", err)
	}
	t.Logf("session response: sid=%s deposit=%s expires=%s",
		resp.Sid, resp.DepositAddress, resp.ExpiresAt)

	if resp.Sid == "" {
		t.Errorf("expected non-empty sid in /session/start response")
	}
	if resp.DepositAddress == "" {
		t.Errorf("expected non-empty depositAddress in /session/start response")
	}
	// addressSignature must be non-empty — we wired
	// IS_ADDRESS_SIGNER_SECRET above so the HMAC stub is active.
	// (Production wires an HSM/KMS asymmetric signer per spec §5.7.)
	if resp.AddressSignature == "" {
		t.Errorf("expected non-empty addressSignature (signer secret was wired)")
	}
	if resp.ExpiresAt == "" {
		t.Errorf("expected non-empty expiresAt in /session/start response")
	}
	if resp.StatusURL == "" {
		t.Errorf("expected non-empty statusUrl in /session/start response")
	}

	// Note: we deliberately do NOT POST /session/status here. The
	// /status endpoint progresses through IS_OBSERVED → ATTESTING
	// → ON_CHAIN, which requires real IS-locks from dashd LLMQ
	// quorums (absent in regtest) and validator BLS attestation
	// gossip (would need setValidatorSet + per-magi-node PoP
	// wiring). Future work — see MEMORY/dash_is_login_audit_loop.
}
