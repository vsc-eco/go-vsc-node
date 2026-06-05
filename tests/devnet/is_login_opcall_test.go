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
)

// reverseHexBytes flips the byte order of a hex-encoded buffer. Dash/
// Bitcoin txids display as the little-endian (reversed) form of the
// raw sha256d hash bytes; contract-internal state keys use the raw
// (non-reversed) hex. Use this to translate between the two views.
func reverseHexBytes(hexStr string) string {
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return hexStr
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b)
}

// TestIsLoginOpCallSmoke exercises the op=call IS-login path
// end-to-end against a real devnet. Closes the only state-machine
// path the original auth-only smoke test didn't cover.
//
// Flow (~210s total):
//  1. Devnet up (5 magi nodes + dashd regtest)
//  2. Deploy: dash-mapping-contract + dash-forwarder-contract +
//     call-tss (as the op=call target with an `execute`/`setString`
//     surface)
//  3. Wire admin actions on mapping contract:
//       - registerPublicKey (bridge TSS pubkeys; same hex the IS
//         service uses so contract-side deposit re-derivation
//         matches the address handed to the user)
//       - seedBlocks (baseline dashd header)
//       - setForwarderContractId (the just-deployed forwarder)
//       - setAllowedTargetImmediate (call-tss; testnet-only
//         shortcut around the 7-day timelock)
//       - setValidatorSet (real PoP from magi-1 BLS)
//       - setMinAttestations(1)
//  4. Fund the IS service's L2 did:pkh with HBD via L1→L2 deposit
//  5. Start IS service (Vault Transit OFF, HMAC stub for the test
//     signer, real L2 submitter + dashd watcher + libp2p gossip
//     into magi-1)
//  6. /session/start with Op="call", Args={Contract: call-tss-id,
//     Method: "setString", ArgsB64: base64("ophk,opval"),
//     AmountDuffs: 0}
//  7. SendDashTo to the deposit address — real dashd payment
//  8. Watcher fires → orchestrator drives ATTESTING → gossip
//     attestation from magi-1 → real L2 mapInstantSendV2 →
//     contract's HandleMapInstantSendV2 → dispatchForward →
//     forwarder.execute → call-tss.setString
//  9. Assert call-tss state has the expected key/value (proving
//     the forward-dispatch reached the target)
//
// The setAllowedTargetImmediate enabler (commit 7188e11) is what
// makes this test feasible — without it the test would need 86,400
// regtest blocks of mining to clear the AllowListGovernanceTimelock-
// Blocks cooldown.
//
// Run: go test -v -run TestIsLoginOpCallSmoke -timeout 20m ./tests/devnet/
func TestIsLoginOpCallSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping op=call devnet smoke in short mode")
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
	t.Logf("mapping wasm: %s", mappingWasm)
	t.Logf("forwarder wasm: %s", forwarderWasm)

	// Build call-tss locally (it's the test-only target contract
	// already used by other devnet tests for non-IS-login flows).
	callTssWasm, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("BuildCallTssContract: %v", err)
	}
	t.Logf("call-tss wasm: %s", callTssWasm)

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

	// Mine 110 dashd blocks for COINBASE_MATURITY + seed baseline.
	if _, err := d.MineDashBlocks(ctx, 110); err != nil {
		t.Fatalf("mining initial dash blocks: %v", err)
	}
	seedHeader, err := d.GetDashBlockHeaderHex(ctx, 1)
	if err != nil {
		t.Fatalf("reading seed header: %v", err)
	}

	// === Deploy contracts ===
	t.Log("deploying dash-mapping-contract...")
	mappingId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     mappingWasm,
		Name:         "dash-mapping-contract",
		Description:  "op=call test mapping contract",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploying mapping contract: %v", err)
	}
	t.Logf("mapping deployed: %s", mappingId)

	t.Log("deploying dash-forwarder-contract...")
	forwarderId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     forwarderWasm,
		Name:         "dash-forwarder-contract",
		Description:  "op=call test forwarder",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploying forwarder contract: %v", err)
	}
	t.Logf("forwarder deployed: %s", forwarderId)

	t.Log("deploying call-tss (op=call target)...")
	targetId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     callTssWasm,
		Name:         "call-tss-target",
		Description:  "op=call test target",
		DeployerNode: 1,
	})
	if err != nil {
		t.Fatalf("deploying call-tss: %v", err)
	}
	t.Logf("call-tss deployed: %s", targetId)

	// === Init forwarder + wire mapping admin actions ===
	if _, err := d.CallContract(ctx, 1, forwarderId, "init", mappingId); err != nil {
		t.Fatalf("forwarder.init: %v", err)
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

	// seedBlocks.
	seedPayload := fmt.Sprintf(`{"block_header":"%s","block_height":1}`, seedHeader)
	if _, err := d.CallContract(ctx, 1, mappingId, "seedBlocks", seedPayload); err != nil {
		t.Fatalf("seedBlocks: %v", err)
	}

	// registerPublicKey.
	regPayload := fmt.Sprintf(
		`{"primary_public_key":%q,"backup_public_key":%q}`,
		primaryPub, backupPub)
	if _, err := d.CallContract(ctx, 1, mappingId, "registerPublicKey", regPayload); err != nil {
		t.Fatalf("registerPublicKey: %v", err)
	}

	// setForwarderContractId — pins the just-deployed forwarder as
	// the trusted dispatcher.
	if _, err := d.CallContract(ctx, 1, mappingId, "setForwarderContractId", forwarderId); err != nil {
		t.Fatalf("setForwarderContractId: %v", err)
	}

	// setAllowedTargetImmediate — testnet-only shortcut around the
	// 7-day allowlist timelock (commit 7188e11 enabler).
	if _, err := d.CallContract(ctx, 1, mappingId, "setAllowedTargetImmediate", targetId); err != nil {
		t.Fatalf("setAllowedTargetImmediate: %v", err)
	}

	// setValidatorSet + setMinAttestations.
	//
	// IDEAL: register all 5 magi nodes so EVERY attestation response
	// (mesh-propagated to magi-1..5 over the gossipsub topic) is
	// acceptable. Pragmatic: only magi-1's identityConfig.json is
	// reliably readable from the test side at this point — magi-2..5's
	// BLS PoPs computed off their identityConfig.json fail to verify
	// at setValidatorSet (PoP validation aborts the whole call → vs-0
	// never indexed → test t.Fatal at vs-poll). Investigation needed:
	// is data-N/config/identityConfig.json populated for N=2..5 at
	// devnet-start, or only on first block-produce? Cross-ref devnet-
	// setup logic. Until then we register magi-1 only and document the
	// known flake: occasionally magi-2..5 win the gossipsub response
	// race and the IS service drops their attestations as "unknown
	// validator DID" → ATTESTATION_TIMEOUT.
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

	// Pre-fund the IS service L2 account.
	if err := d.FundL2Account(ctx, l2DID, "100.000", 12*time.Second); err != nil {
		t.Fatalf("FundL2Account: %v", err)
	}

	// Poll L2 GQL until setValidatorSet has been INDEXED (key "vs-0"
	// reachable via getStateByKeys). The IS service caches its
	// validator-set view for 30s on first lookup (main.go:300:
	// cacheTTL=30*time.Second). If the IS service queries L2 GQL
	// BEFORE the contract's vs-0 state is indexed, it caches an
	// empty validator set — every subsequent attestation from
	// magi-1 is then rejected with "attestation from unknown
	// validator DID; dropping" for the next 30s. That's the flake
	// (~60% rate on 8 prior runs) that masked the actual op=call
	// pipeline behaviour. Polling here makes the validator-set
	// indexing deterministic so the IS service caches a correct
	// view.
	const vsKey = "vs-0"
	// 60s — observed runs where L2 indexing of setValidatorSet takes
	// 30-45s under op=call's heavier setup load (more L2 contract
	// deploys + calls before the IS service starts). 30s was tight
	// enough to flake on ~30% of runs.
	const vsPollTimeout = 60 * time.Second
	vsDeadline := time.Now().Add(vsPollTimeout)
	vsIndexed := false
	for time.Now().Before(vsDeadline) {
		st, qerr := d.GetStateByKeys(ctx, 1, mappingId, []string{vsKey})
		if qerr == nil && st != nil {
			if v, ok := st[vsKey].(string); ok && v != "" {
				t.Logf("validator-set indexed at L2 GQL: vs-0=%q…", v[:min(80, len(v))])
				vsIndexed = true
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while waiting for validator-set indexing")
		case <-time.After(2 * time.Second):
		}
	}
	if !vsIndexed {
		t.Fatalf("setValidatorSet not indexed via L2 GQL within %s — IS service would cache empty validator set and reject all attestations", vsPollTimeout)
	}

	// === Start IS service ===
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
		AddressSignerSecret:   "opcall-smoke-hmac-dev-only",
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

	// Let the gossipsub mesh form between IS service + magi-1 +
	// any onward connections. GossipSub heartbeat is 1s default;
	// mesh formation takes ~3 heartbeats nominally, but in the op=
	// call test the IS service must also have its per-topic
	// subscription propagated to magi-1's PubSub layer before the
	// first BroadcastRequest can be heard. Under L2 churn from all
	// the setup calls preceding StartIsService, this can take
	// 20s+ — measured flakes (collected=0 collected=0) at 10s
	// (~50%) + 20s (~30%). Bumped to 30s as the empirical sweet
	// spot. If this still flakes, the next step is a deterministic
	// poll on the IS service's /healthz endpoint until
	// connectedPeers ≥ 1 + topic subscription confirmed.
	t.Log("waiting 30s for gossipsub mesh to form...")
	select {
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		t.Fatal("ctx cancelled waiting for mesh")
	}

	// === /session/start with op=call ===
	//
	// call-tss's setString expects "key,value" raw bytes in the
	// input. Base64-encode it for the wire.
	const targetKey = "ophk"
	const targetVal = "opval"
	argsB64 := base64.StdEncoding.EncodeToString([]byte(targetKey + "," + targetVal))
	resp, err := d.IsStartSession(ctx, IsSessionStartReq{
		Op: "call",
		Args: map[string]interface{}{
			"contract": targetId,
			"method":   "setString",
			"args":     argsB64,
			"amount":   0, // value-less op=call (dust floor applies)
		},
	})
	if err != nil {
		t.Fatalf("/session/start: %v", err)
	}
	t.Logf("op=call session: sid=%s deposit=%s requiredAmount=%d",
		resp.Sid, resp.DepositAddress, resp.RequiredAmountDuffs)
	if resp.DepositAddress == "" {
		t.Fatal("empty depositAddress")
	}

	// === Pay Dash to deposit address ===
	t.Logf("paying real Dash to %s...", resp.DepositAddress)
	dashTxId, err := d.SendDashTo(ctx, resp.DepositAddress, "0.001")
	if err != nil {
		t.Fatalf("SendDashTo: %v", err)
	}
	t.Logf("dashd tx: %s", dashTxId)

	// === Pre-seed the sender DashDID's contract-internal HBD ===
	//
	// mapping.dispatchForward (spec §5.2.6) debits ~500 milli-HBD of
	// RC-reimbursement from the SENDER's internal HBD balance BEFORE
	// invoking the forwarder. A fresh DashDID — every first-time
	// user — has zero internal HBD on the mapping contract, so the
	// pre-check fails with StatusForwardFailedInsufficientRC and the
	// forwarder dispatch never runs. In production the sender funds
	// their internal HBD via a prior DASH→HBD swap; for this devnet
	// test we use the regtest-only seedInternalHbd admin enabler
	// (mapping main.go:seedInternalHbd, gated by IsRegtest(NetworkMode))
	// to credit the DashDID directly. Both the dispatch HBD pre-check
	// AND this seed enabler are absent on mainnet.
	//
	// We resolve the sender by walking back one hop through the
	// dashd verbose RPC — dashd's wallet picks UTXO inputs from
	// whichever wallet address has spendable coins. The DashDID is
	// `did:pkh:bip122:<regtest genesis>:<first-input-address>`,
	// matching the mapping contract's ResolveSenderDashDID derivation.
	senderAddr, err := d.ResolveDashTxSenderAddress(ctx, dashTxId)
	if err != nil {
		t.Fatalf("resolving sender of dash tx %s: %v", dashTxId, err)
	}
	senderDID := DashTestnetDIDFromAddress(senderAddr)
	t.Logf("sender DashDID: %s", senderDID)

	// 2000 milli-HBD = 2 HBD — comfortably above the 500-milli
	// dispatchForward pre-check budget so a future RC-cost bump
	// doesn't make the test flaky.
	const seedAmountMilliHbd = 2000
	seedHbdPayload := fmt.Sprintf("%s,%d", senderDID, seedAmountMilliHbd)
	seedTxId, err := d.CallContract(ctx, 1, mappingId, "seedInternalHbd", seedHbdPayload)
	if err != nil {
		t.Fatalf("seedInternalHbd(%s, %d): %v", senderDID, seedAmountMilliHbd, err)
	}
	t.Logf("seeded %d milli-HBD for sender DashDID (dispatchForward pre-check budget), L1 tx=%s",
		seedAmountMilliHbd, seedTxId)

	// Poll the contract-internal HBD balance until the seed call has
	// been indexed via L2 GQL. Confirms the L1→L2 broadcast actually
	// committed before the IS-service-submitted mapInstantSendV2 runs.
	// The balance is stored as packed-uint64 bytes (non-UTF8), so we
	// must use GetStateByKeysHex to round-trip through JSON cleanly.
	seedBalanceKey := "a-hbd/" + senderDID
	const seedBalancePollTimeout = 90 * time.Second
	seedBalanceDeadline := time.Now().Add(seedBalancePollTimeout)
	seedBalanceIndexed := false
	for time.Now().Before(seedBalanceDeadline) {
		bal, qerr := d.GetStateByKeysHex(ctx, 1, mappingId, []string{seedBalanceKey})
		if qerr == nil && bal != nil {
			if v, ok := bal[seedBalanceKey].(string); ok && v != "" {
				t.Logf("seed balance indexed at L2 GQL: %s=%s (hex)", seedBalanceKey, v)
				seedBalanceIndexed = true
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while waiting for seed-balance indexing")
		case <-time.After(3 * time.Second):
		}
	}
	if !seedBalanceIndexed {
		// Diagnostic: query the L1 → L2 tx status. If the L1 tx was
		// indexed by the L2 streamer it'd have a transactions row.
		if status, terr := d.FindTransactionStatus(ctx, 1, seedTxId); terr == nil {
			t.Logf("seed L1 tx status: %s", status)
		}
		if outs, oerr := d.FindContractOutputByInput(ctx, 1, seedTxId); oerr == nil {
			t.Logf("seed contract-output records on miss: %+v", outs)
		}
		t.Fatalf("seedInternalHbd did not produce a queryable balance at %s within %s — the L1 broadcast may have been rejected OR the contract aborted silently OR the L2 streamer hasn't processed it yet. Cannot continue safely; mapInstantSendV2 would hit InsufficientRC.", seedBalanceKey, seedBalancePollTimeout)
	}

	// === Drive through the state machine ===
	if _, err := d.WaitForIsSessionState(ctx, resp.Sid,
		[]string{"IS_OBSERVED", "ATTESTING", "L2_SUBMITTED", "ON_CHAIN"},
		90*time.Second); err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs (waiting for IS_OBSERVED):\n%s", isLogs)
		}
		t.Fatalf("session did not reach IS_OBSERVED: %v", err)
	}

	final, err := d.WaitForIsSessionState(ctx, resp.Sid,
		[]string{"L2_SUBMITTED", "ON_CHAIN"},
		120*time.Second)
	if err != nil {
		if isLogs, lerr := d.IsServiceLogs(ctx); lerr == nil {
			t.Logf("is-service logs (final-state wait):\n%s", isLogs)
		}
		// Also dump magi-1 logs — the witness is where attestation
		// signatures originate; if magi-1 is silent the gossip path
		// is the bug, not the IS service.
		if magi1Logs, lerr := d.Logs(ctx, "magi-1"); lerr == nil {
			t.Logf("magi-1 logs (final-state wait):\n%s", truncateLogs(magi1Logs, 80))
		}
		t.Fatalf("session did not reach L2_SUBMITTED/ON_CHAIN: %v", err)
	}
	t.Logf("op=call session advanced: state=%s l2TxId=%s dashTxId=%s",
		final.State, final.L2TxId, final.DashTxId)
	if final.L2TxId == "" {
		t.Errorf("expected non-empty l2TxId after L2 submission, got empty")
	}

	// === Assert the target's state was modified ===
	//
	// The full pipeline writes:
	//   dashd payment → orchestrator → L2 mapInstantSendV2 → mapping.
	//   HandleMapInstantSendV2 → dispatchForward → forwarder.Execute
	//   → call-tss.setString → sdk.StateSetObject("ophk","opval")
	// We poll the L2 GQL getStateByKeys on the target until it sees
	// the write OR the deadline fires.
	//
	// The sender's internal HBD was pre-seeded above so the
	// dispatchForward HBD pre-check passes and the forwarder
	// dispatch actually runs. Pre-seeding plus the forwarder's
	// b64-decode fix (utxo-mapping commit 4d8253a) close the
	// op=call full-pipeline E2E gap.
	const expectedTargetVal = targetVal
	statePollDeadline := time.Now().Add(90 * time.Second)
	stateOk := false
	var lastObserved any
	for time.Now().Before(statePollDeadline) {
		st, err := d.GetStateByKeys(ctx, 1, targetId, []string{targetKey})
		if err == nil && st != nil {
			lastObserved = st[targetKey]
			if v, ok := st[targetKey].(string); ok && v == expectedTargetVal {
				stateOk = true
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while polling target contract state")
		case <-time.After(3 * time.Second):
		}
	}
	if !stateOk {
		// Diagnostics on failure: dump (a) the forwardQueue status
		// entry for this dash txid — this tells us WHERE in the
		// dispatch chain the call stopped (PENDING_FORWARD =
		// dispatch never invoked forwarder; FORWARD_FAILED =
		// allow-list check or HBD pre-check rejected; FORWARD_
		// FAILED_INSUFFICIENT_RC = HBD pre-fund didn't take effect;
		// FORWARDED = forwarder ran but target write missed); (b)
		// the allowedTargets entry for the target contract — if
		// missing, setAllowedTargetImmediate aborted silently (the
		// regtest/testnet build-mode gate; dev.wasm should accept,
		// testnet.wasm rejects); (c) the sender's contract-internal
		// HBD balance — confirms the seedInternalHbd call landed.
		// State key shapes mirror the mapping contract's constants
		// (DirPathDelimiter is "-", NOT "/"):
		//   forwardQueue:    "fq-<internalTxid>"  (ForwardQueueKeyPrefix; see below)
		//   allowedTargets:  "at-<contract-id>"   (AllowedTargetsKeyPrefix)
		//   internalBalance: "a-<asset>/<did>"    (BalancePrefix+asset+"/"+dashDID; see
		//                                          mapping/forwarder_integration.go:484)
		//   forwarderId:     "forwarder"          (ForwarderContractIdStateKey)
		//   primary pubkey:  "pubkey"             (PrimaryPublicKeyStateKey)
		//   backup pubkey:   "backupkey"          (BackupPublicKeyStateKey)
		//
		// internalTxid: the contract's `rawTxId` (mapinstantsend_v2.go:350)
		// returns `hex(sha256d(rawTxBytes))` — i.e. the raw double-SHA256
		// hash, NO byte reversal. dashd's displayed txid is the
		// reverse-byte version of the same hash (Bitcoin/Dash little-
		// endian display convention). To match the contract's key we
		// must byte-reverse dashTxId.
		internalTxid := reverseHexBytes(dashTxId)
		fqKey := "fq-" + internalTxid
		atKey := "at-" + targetId
		ibKey := "a-hbd/" + senderDID
		fwdKey := "forwarder"
		pkKey := "pubkey"
		bkKey := "backupkey"
		if diag, derr := d.GetStateByKeys(ctx, 1, mappingId,
			[]string{fqKey, atKey, ibKey, fwdKey}); derr == nil {
			t.Logf("dispatch diagnostics — forwardQueue[%s]=%v allowedTargets[%s]=%v internalBalance[%s]=%v forwarderId[%s]=%v",
				fqKey, diag[fqKey], atKey, diag[atKey], ibKey, diag[ibKey], fwdKey, diag[fwdKey])
		} else {
			t.Logf("dispatch diagnostics: GetStateByKeys failed: %v", derr)
		}
		// Also query the BLS-bridge public-key state. mapInstantSendV2's
		// loadPublicKeys (main.go:933) calls `*sdk.StateGetObject(...)`
		// UNCONDITIONALLY — if the registerPublicKey call didn't land,
		// the contract panics on nil deref with empty Ret (which is
		// exactly what the L2 contract-output diagnostic shows when
		// mapInstantSendV2 aborts). Bytes are raw 33-byte compressed
		// pubkeys (not UTF-8), so use hex.
		if pkDiag, perr := d.GetStateByKeysHex(ctx, 1, mappingId, []string{pkKey, bkKey}); perr == nil {
			t.Logf("dispatch diagnostics (hex) — primaryPubkey[%s]=%v backupPubkey[%s]=%v",
				pkKey, pkDiag[pkKey], bkKey, pkDiag[bkKey])
		} else {
			t.Logf("dispatch diagnostics (hex pubkeys) failed: %v", perr)
		}
		// Re-query internal balance as hex so the binary packed-uint64
		// value (which GetStateByKeys returns as nil due to JSON
		// encoding) shows its real bytes. A non-nil hex string here
		// confirms the seedInternalHbd state-write actually landed.
		if hexDiag, hderr := d.GetStateByKeysHex(ctx, 1, mappingId, []string{ibKey}); hderr == nil {
			t.Logf("dispatch diagnostics (hex) — internalBalance[%s]=%v", ibKey, hexDiag[ibKey])
		} else {
			t.Logf("dispatch diagnostics (hex) failed: %v", hderr)
		}

		// Query the L2 contract-output(s) for the seedInternalHbd L1 tx
		// and the IS-service's mapInstantSendV2 L2 tx. ContractOutputs
		// batch ALL contract calls in an L2 block — so the inputs[] /
		// results[] arrays let us identify exactly WHICH call in the
		// batch succeeded vs aborted. Cross-reference inputs[i] (txid)
		// with results[i] (ok + ret) to attribute outcomes.
		dumpOutput := func(label, txid string, outs []ContractOutputRecord) {
			if len(outs) == 0 {
				t.Logf("%s (tx=%s): no contract-outputs found", label, txid)
				return
			}
			for i, o := range outs {
				t.Logf("%s (tx=%s) output[%d] id=%s block=%d contract=%s",
					label, txid, i, o.Id, o.BlockHeight, o.ContractId)
				for j, r := range o.Results {
					inputAttribution := "<no input>"
					if j < len(o.Inputs) {
						inputAttribution = o.Inputs[j]
					}
					t.Logf("  call[%d] input=%s ok=%v ret=%q err=%q errMsg=%q",
						j, inputAttribution, r.Ok, r.Ret, r.Err, r.ErrMsg)
				}
			}
		}
		if seedOuts, serr := d.FindContractOutputByInput(ctx, 1, seedTxId); serr == nil {
			dumpOutput("seedInternalHbd batch", seedTxId, seedOuts)
		} else {
			t.Logf("seedInternalHbd contract-output query failed: %v", serr)
		}
		if final.L2TxId != "" {
			if misvOuts, merr := d.FindContractOutputByInput(ctx, 1, final.L2TxId); merr == nil {
				dumpOutput("mapInstantSendV2 batch", final.L2TxId, misvOuts)
			} else {
				t.Logf("mapInstantSendV2 contract-output query failed: %v", merr)
			}
		}
		if magi1Logs, lerr := d.Logs(ctx, "magi-1"); lerr == nil {
			_ = strings.Contains(magi1Logs, "forwarder.execute")
		}
		t.Fatalf("target contract state[%q] did not become %q within 90s "+
			"(last observed: %v). Full op=call pipeline did NOT reach the "+
			"target contract; see dispatch diagnostics above.",
			targetKey, expectedTargetVal, lastObserved)
	}
	t.Logf("target contract state[%q]=%q — full op=call pipeline verified end-to-end",
		targetKey, expectedTargetVal)
}
