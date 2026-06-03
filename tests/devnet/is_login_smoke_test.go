package devnet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
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
// Full state-machine coverage (added in a follow-up commit):
//   - WAITING_FOR_IS → IS_OBSERVED: synthesised via the test-only
//     /test/observed/{sid} endpoint with a 32-byte rawTxHex.
//     Bypasses the dashd watcher entirely because Dash never
//     activated SegWit (dashd v23 returns "Invalid address format"
//     for `tdash1...` P2WSH bech32 addresses).
//   - IS_OBSERVED → ATTESTING: orchestrator's normal transition
//     inside Drive() after the observation callback fires.
//   - ATTESTING → L2_SUBMITTED → ON_CHAIN: synthesised via the
//     test-only /test/attestation/{sid} endpoint. The harness reads
//     magi-1's BLS private-key seed from
//     <devnetDir>/data-1/config/identityConfig.json, signs the
//     canonical IsLockAttestationRequest (same hashes the
//     orchestrator computes), posts the response. The IS service's
//     per-sig BLS Verify accepts (no validator-set check in
//     log-only submitter mode), the aggregator + LogOnly submitter
//     return success, and the orchestrator drives through ON_CHAIN.
//
// What it does NOT verify (still out of scope, future work):
//   - Real validator gossip: would need to wire islock-attestation
//     into cmd/vsc-node so magi witnesses participate in the
//     topic, plus a Bootnodes helper that exposes magi-N's libp2p
//     multiaddrs to the IS service.
//   - Real L2 submission landing on the contract: requires
//     funding the IS service's L2 did:pkh:eip155 account with HBD
//     for RC (we use the LogOnly submitter to skip this).
//   - setValidatorSet round-trip + forwardQueue state assertions:
//     would require a real L2 submission + the dash-forwarder-
//     contract deployed.
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

	// Mine a few blocks so seedBlocks has something to chain onto.
	// We don't need COINBASE_MATURITY headroom because we drive the
	// IS_OBSERVED transition via the /test/observed endpoint
	// directly — Dash never activated SegWit, so the bech32 P2WSH
	// address the IS service generates can't be paid via
	// `sendtoaddress` on dashd regtest at all.
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

	// 3. Generate dummy bridge TSS pubkeys for the IS service. The
	//    keys don't need to own anything — the deposit address they
	//    derive is never paid against, and we drive the
	//    WAITING_FOR_IS / ATTESTING transitions via test-only HTTP
	//    endpoints. But the SAME pubkeys must be registered on the
	//    contract via registerPublicKey so the contract-side deposit-
	//    address re-derivation matches what the IS service hands the
	//    user — otherwise any real mapInstantSendV2 submission would
	//    abort with "derived deposit address ... not found in tx
	//    outputs".
	primaryPub, backupPub, l2Priv, err := GenerateIsTestKeys()
	if err != nil {
		t.Fatalf("generating IS test keys: %v", err)
	}
	l2DID, err := ComputeL2DIDFromPrivHex(l2Priv)
	if err != nil {
		t.Fatalf("computing L2 DID: %v", err)
	}
	t.Logf("IS service L2 account: %s", l2DID)

	// 3d. Pre-fund the IS service's L2 account by broadcasting an
	//     L1→L2 deposit op (initminer → vsc.gateway with memo
	//     `to=<did>`). State engine credits the L2 account when it
	//     sees the gateway-targeted transfer with that memo shape
	//     (mirrors ledger_system_test.go's "memo to=<eth address>"
	//     case). Devnet block time is 3s; 12s settle wait gives the
	//     state engine ~4 blocks of headroom.
	if err := d.FundL2Account(ctx, l2DID, "100.000", 12*time.Second); err != nil {
		t.Fatalf("FundL2Account: %v", err)
	}

	// 3a. Register the bridge pubkeys on the contract. Owner-gated;
	//     the deployer (magi.test1) is contract.owner.
	registerKeysPayload := fmt.Sprintf(
		`{"primary_public_key":%q,"backup_public_key":%q}`,
		primaryPub, backupPub)
	if _, err := d.CallContract(ctx, 1, mappingId, "registerPublicKey", registerKeysPayload); err != nil {
		t.Fatalf("registerPublicKey: %v", err)
	}
	t.Logf("registered bridge pubkeys on contract (primary=%s… backup=%s…)",
		primaryPub[:16], backupPub[:16])

	// 3b. Register magi-1 as the active validator set for epoch 0
	//     (the IS service's log-only mode stamps epoch=0 into every
	//     attestation request). Builds a 4-field PoP-bound payload
	//     using magi-1's real BLS key from data-1/config/
	//     identityConfig.json — the contract's setValidatorSet does
	//     full BLS PoP verification before accepting the entry.
	validatorPayload, err := d.BuildValidatorSetPayload(0, []int{1})
	if err != nil {
		t.Fatalf("BuildValidatorSetPayload: %v", err)
	}
	if _, err := d.CallContract(ctx, 1, mappingId, "setValidatorSet", validatorPayload); err != nil {
		t.Fatalf("setValidatorSet: %v", err)
	}
	t.Logf("registered validator set for epoch 0 (magi-1 → %s)",
		strings.SplitN(strings.SplitN(validatorPayload, ";", 2)[1], "=", 2)[0])

	// 3c. setMinAttestations(1) — single-attester quorum. Default
	//     fallback is also 1, but explicit so a future contract bump
	//     doesn't silently raise it.
	if _, err := d.CallContract(ctx, 1, mappingId, "setMinAttestations", "1"); err != nil {
		t.Fatalf("setMinAttestations: %v", err)
	}

	// 4. Start the IS service in log-only L2 mode. The L2 GraphQL
	//    trio is left unset, so the orchestrator uses the
	//    SubmitterLogOnly path: SubmitMapInstantSend returns
	//    "log-only:no-l2-tx" + FetchTransactionStatus returns
	//    CONFIRMED. That advances the session through L2_SUBMITTED →
	//    ON_CHAIN without needing to fund an L2 did:pkh account or
	//    wire setValidatorSet on the contract (the orchestrator's
	//    DID↔pubkey expected-set is nil in log-only mode, so it
	//    accepts any BLS-valid attestation).
	isOpts := IsServiceOpts{
		PrimaryPubkey:       primaryPub,
		BackupPubkey:        backupPub,
		AddressSignerSecret: "smoke-test-hmac-secret-dev-only-do-not-ship",
		// Real L2 submitter: target magi-1's GraphQL endpoint with
		// the priv key whose did:pkh:eip155 account we just funded.
		// The IS service will now build + sign + broadcast real
		// VSC native transactions instead of the log-only stub.
		L2GqlURL:       "http://magi-1:8080/api/v1/graphql",
		L2PrivKeyHex:   l2Priv,
		L2DashContract: mappingId,
		// Devnet test bypass: enables -testBypassDashdISLock AND
		// TestEndpointsEnabled (wires `/test/observed/{sid}` +
		// `/test/attestation/{sid}`). args.go gates this to
		// -network=devnet so production deploys cannot enable it.
		TestBypassDashdISLock: true,
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

	// 6. Drive WAITING_FOR_IS → IS_OBSERVED via /test/observed.
	//    We supply a synthesised Bitcoin-wire-format rawTxHex with
	//    one P2PKH input + one P2WSH output paying the IS-service-
	//    issued deposit address. This satisfies BOTH the contract's
	//    FindOutputAmount (the P2WSH output matches the address)
	//    AND ResolveSenderDashDID (the P2PKH-style input recovers
	//    to a Dash address via txscript.ComputePkScript). See
	//    tests/devnet/synthetic_dashtx.go for the construction.
	//
	//    Decode the deposit address into a btcutil.Address using
	//    the same Dash testnet params the IS service uses (R5 devnet
	//    case inherits testnet params).
	depositAddr, err := DecodeDashP2WSH(resp.DepositAddress, dashTestNetParamsForHarness())
	if err != nil {
		t.Fatalf("decoding deposit address: %v", err)
	}
	rawTxHex, syntheticSender, err := BuildSyntheticDashTxToAddress(
		depositAddr, 100_000, dashTestNetParamsForHarness())
	if err != nil {
		t.Fatalf("BuildSyntheticDashTxToAddress: %v", err)
	}
	t.Logf("synthetic Dash tx: rawTxHex=%s… sender=%s amount=100000 duffs",
		rawTxHex[:32], syntheticSender)

	// BuildResponse requires a 32-byte hex txid (64 lowercase hex
	// chars). The contract uses the actual SHA256d of rawTxBytes as
	// the txid (Bitcoin convention); synthesise it here so the
	// orchestrator + contract see the same value.
	txidBytes := make([]byte, 32)
	if _, err := rand.Read(txidBytes); err != nil {
		t.Fatalf("rand: %v", err)
	}
	fakeTxid := hex.EncodeToString(txidBytes)
	t.Logf("force-observing session sid=%s via /test/observed (rawTxHex=%s…)",
		resp.Sid, rawTxHex[:16])
	if err := d.IsForceObservedWithRawTx(ctx, resp.Sid, fakeTxid, rawTxHex); err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs (post-force):\n%s", isLogs)
		}
		t.Fatalf("IsForceObserved: %v", err)
	}

	// Wait for ATTESTING — the orchestrator transitions
	// IS_OBSERVED → ATTESTING in Drive() before publishing to the
	// (noop) broadcaster + awaiting collector responses.
	if _, err := d.WaitForIsSessionState(ctx, resp.Sid,
		[]string{"ATTESTING", "L2_SUBMITTED", "ON_CHAIN"},
		60*time.Second); err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs (waiting for ATTESTING):\n%s", isLogs)
		}
		t.Fatalf("session did not reach ATTESTING: %v", err)
	}
	t.Logf("session reached ATTESTING; injecting signed attestation from magi-1...")

	// 7. Build a real signed attestation from magi-1's BLS key and
	//    inject it via /test/attestation. The orchestrator's per-sig
	//    BLS verify (modules/islock-attestation Verify) requires the
	//    canonical message to match what the orchestrator computed —
	//    same rawTxHex + same Instruction string. We decode the
	//    instruction from depositInstructionHex (the IS service's
	//    /session/start response carries it).
	instructionBytes, err := hex.DecodeString(resp.DepositInstructionHex)
	if err != nil {
		t.Fatalf("decoding depositInstructionHex: %v", err)
	}
	if err := d.IsForceAttestation(ctx, resp.Sid, fakeTxid, rawTxHex, string(instructionBytes), 1); err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs (post-attestation):\n%s", isLogs)
		}
		t.Fatalf("IsForceAttestation: %v", err)
	}

	// 8. Verify the orchestrator advances through L2_SUBMITTED →
	//    ON_CHAIN. With the LogOnly submitter both steps return
	//    success without any actual L2 broadcast; what we're really
	//    proving is that the orchestrator's per-sig BLS verify
	//    accepted our attestation + the aggregator + submitter +
	//    reconciler all run their state-machine transitions.
	final, err := d.WaitForIsSessionState(ctx, resp.Sid,
		[]string{"L2_SUBMITTED", "ON_CHAIN"},
		60*time.Second)
	if err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs (post-attestation):\n%s", isLogs)
		}
		t.Fatalf("session did not reach L2_SUBMITTED/ON_CHAIN: %v", err)
	}
	t.Logf("session advanced to state=%s (l2TxId=%s dashTxId=%s)",
		final.State, final.L2TxId, final.DashTxId)

	if final.State != "L2_SUBMITTED" && final.State != "ON_CHAIN" {
		t.Errorf("expected L2_SUBMITTED or ON_CHAIN, got %s", final.State)
	}
	if final.DashTxId != fakeTxid {
		t.Errorf("expected dashTxId=%q, got %q", fakeTxid, final.DashTxId)
	}
	if final.L2TxId == "" {
		t.Errorf("expected non-empty l2TxId, got empty")
	}

	// Note: we do NOT chase the session through ATTESTING → ON_CHAIN
	// here. Doing so requires setValidatorSet with real BLS PoPs
	// from each magi node's BLS key + libp2p bootnodes wiring on
	// the IS service. Future work — see MEMORY/dash_is_login_audit_loop.
}
