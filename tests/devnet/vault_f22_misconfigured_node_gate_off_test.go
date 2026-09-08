package devnet

// vault_f22_misconfigured_node_gate_off_test.go, F22: a MISCONFIGURED node runs
// with the S3 output-scoping theft gate silently OFF.
//
// What this documents. The gate that stops a RETIRING vault generation's key from
// signing anything other than a successor-paying evacuation sweep is scoped by
// modules/tss/solvency_gate.go isBtcVaultKey:
//
//	btcContract := tssMgr.sconf.OracleParams().ContractId("BTC")
//	return btcContract != "" && strings.HasPrefix(keyId, btcContract+"-")
//
// An EMPTY BTC contract id therefore makes btcSignRefused return false for every
// key, so a node whose oracle configuration lacks the BTC contract id issues the
// keysign that its correctly configured peers refuse. The gate does not warn, does
// not fail closed and does not stop the node from participating in consensus: it is
// simply inert on that node. That is a fail-OPEN, and it is what this test records.
//
// The design observation (recorded as INFO, not as a pass): the fleet is still
// protected while only a MINORITY is misconfigured, because a BTC keysign needs
// ceil(2n/3) parties (4 of 5 here) and the 3 correctly configured nodes refuse to
// issue, so the 2 willing parties can never reach threshold. The protection is the
// signing THRESHOLD, not the gate; if a majority of a committee were misconfigured
// the retiring key would sign an arbitrary digest with nobody logging a refusal.
// A per-node configuration mistake therefore silently erodes an anti-theft control,
// which argues for a startup assertion (refuse to run, or at least WARN loudly, when
// a BTC vault key exists on a network whose BTC contract id is unset).
//
// HOW THE MISCONFIGURATION IS PRODUCED (read from source on 2026-09-05, this is a
// deliberate deviation from the literal spec wording and it is load bearing):
//
//   - The per-node file tests/devnet/oracle.go writes, devnet-data/data-N/config/
//     oracleConfig.json, carries ONLY RPC connection details (modules/oracle/config.go
//     oracleConfig: Chains -> RpcHost/RpcUser/RpcPass). It contains no contract id at
//     all, so removing its BTC entry alone would NOT turn the gate off.
//   - The contract id isBtcVaultKey reads comes from OracleParams.ChainContracts in
//     the -sysconfig override file, which tests/devnet/compose.go writes ONCE as
//     devnet-data/sysconfig.json and bind mounts into EVERY container at the same
//     path, so it cannot be made per node by writing a file.
//   - cmd/vsc-node/main.go loads that file once at startup (LoadOverrides, main.go
//     line 131), long before the node starts processing blocks. So the split is made
//     in TIME rather than in space: write the shared sysconfig with an empty BTC
//     entry, restart ONLY nodes 4 and 5 so they boot with ContractId("BTC") == "",
//     then restore the file on disk while nodes 1, 2 and 3 keep the correct value they
//     already hold in memory. Both mutations are applied (the per-node oracleConfig
//     BTC entry is removed for nodes 4 and 5 as well) so the misconfigured pair is
//     misconfigured in exactly the way an operator would be.
//
// Both consensus safety checks that also read the BTC contract id are inert during
// the window: IsBondLockedRetiringMember (bond_lock.go) only decides a consensus
// unstake outcome and this test issues none, and vaultCheckSigDigest (state_engine.go)
// only fires on a key created->active flip, which happens before the window opens.
// The timed out rogue session on nodes 4 and 5 blames 3 culprits, which is above
// maxBlamed = 5 - (GetThreshold(5)+1) = 1, so tss.go suppresses it as systemic and no
// blame commitment is recorded that could poison the positive control.
//
// Cases:
//
//	F22-NOSIG     the rogue digest for the RETIRING key is signed on NO node: the 3
//	              honest refusals leave 2 willing parties and the threshold needs 4.
//	F22-GATE-OFF  INFO (not a pass): nodes 1..3 log the output-scoping refusal, nodes
//	              4..5 log it zero times and instead build sign sessions for that key.
//	F22-CONTROL   MANDATORY positive control after the configuration is restored: a
//	              LEGITIMATE successor sweep for the same retiring key signs and
//	              settles, so "no signature" above cannot mean "signing was broken".
//	F22-IDENT     contract state byte identical across all 5 nodes at the end.
//
// The rogue row is left in place for the rest of the run (as in F13) so every later
// sign tick re-refuses it; that also yields the post-restore refusal delta on nodes 4
// and 5, which is the evidence that the configuration, and nothing else, was what
// turned their gate off.
//
//	VAULT_F22_RUN=1 go test -v -run TestVaultF22MisconfiguredNodeGateOff -timeout 95m ./tests/devnet/

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"go.mongodb.org/mongo-driver/bson"
)

// vfF22SessionLog is the per sign session line RunActions emits for EVERY SignAction
// it did NOT refuse (modules/tss/tss.go, the "signing gossip readiness set" Verbose
// right after the sessionId is built and before any participant filtering). Its
// sessionId is "sign-<bh>-<idx>-<keyId>", so the key id appears in the same line and
// a match proves that node reached sign-session construction for that key.
const vfF22SessionLog = "signing gossip readiness set"

// vfF22DispatchLog is the later line from SignDispatcher.Start (modules/tss/
// dispatcher.go "sign dispatcher start"), emitted once the node actually starts its
// local signing party. tssTestConfig runs the tss module at trace, so both lines are
// visible in the container logs.
const vfF22DispatchLog = "sign dispatcher start"

func TestVaultF22MisconfiguredNodeGateOff(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F22_RUN") == "" {
		t.Skip("set VAULT_F22_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(85*time.Minute))
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// v2 activation pinned AFTER genesis (~block 190) so gen-0 mints on the v2-off
	// genesis path and the flag is live well before the rotation.
	const hpin = 400

	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	// The post-injection wait is measured in SIGN TICKS read from the config, not in
	// wall clock: TSS only selects signing requests when bh%signInterval == 0.
	signInterval := cfg.SysConfigOverrides.TssParams.SignInterval
	if signInterval == 0 {
		t.Fatalf("PRECONDITION FAILED: tssTestConfig left TssParams.SignInterval at 0, so the post-injection wait has no instrument")
	}

	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)
	c := &vfCase{t: t}
	nodes := vfAllNodes(5)
	gated := []int{1, 2, 3}
	misconf := []int{4, 5}

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault f22 misconfigured node gate off")
	cid := env.cid
	retiringKeyId := cid + "-" + btcvault.VaultKeyName(0)

	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// A RETIRING generation must exist, otherwise the gate under test never applies
	// and the whole experiment is vacuous.
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: the gen-0 to gen-1 rotation did not complete (primary1=%q), so there is no retiring generation whose gate can be turned off", primary1)
	}
	if st := vfVaultStatusOn(d, ctx, 2, cid, 0); st != int(btcvault.VaultStatusRetiring) {
		t.Fatalf("PRECONDITION FAILED: gen-0 status is %d, want %d (Retiring); the output-scoping gate only refuses for a superseded generation", st, int(btcvault.VaultStatusRetiring))
	}
	if st := vfVaultStatusOn(d, ctx, 2, cid, 1); st != int(btcvault.VaultStatusActive) {
		t.Fatalf("PRECONDITION FAILED: gen-1 status is %d, want %d (Active); without a single Active successor the registry resolves UNRESOLVABLE and every keysign is refused for the wrong reason", st, int(btcvault.VaultStatusActive))
	}
	gen0Before := genUtxoCount(t, d, ctx, cid, 0)
	if gen0Before <= 0 {
		t.Fatalf("PRECONDITION FAILED: gen-0 holds %d UTXOs, so the mandatory positive control would have nothing to sweep", gen0Before)
	}
	// The sign selector marks a request FAILED for a non-active key BEFORE
	// btcSignRefused ever runs (tss.go around line 592), so an inactive retiring key
	// would make both the refusal and the fail-open unobservable.
	for _, n := range nodes {
		ks, err := d.GetTssKeys(ctx, n, bson.M{"id": retiringKeyId})
		if err != nil || len(ks) == 0 {
			t.Fatalf("PRECONDITION FAILED: magi-%d has no tss_keys row for %s (err=%v)", n, retiringKeyId, err)
		}
		if ks[0].Status != "active" {
			t.Fatalf("PRECONDITION FAILED: magi-%d reports %s status=%q, not \"active\"; the sign selector would fail the rogue request before the gate, making the observation vacuous", n, retiringKeyId, ks[0].Status)
		}
	}
	t.Logf("PRECONDITION OK: gen-0 Retiring with %d UTXOs, gen-1 Active, retiring key %s active on all 5 nodes", gen0Before, retiringKeyId)

	// ---- 1. misconfigure nodes 4 and 5 ----
	heightsBefore := vfF22ProcessedHeights(t, d, ctx, misconf)
	vfF22WriteOracleConfigWithoutBtc(t, d, misconf)
	if err := d.SetOracleContractIDs(map[string]string{"BTC": ""}); err != nil {
		t.Fatalf("PRECONDITION FAILED: could not write the BTC-less sysconfig override: %v", err)
	}
	t.Logf("F22: wrote an empty ChainContracts BTC entry into the shared sysconfig and dropped the BTC entry from magi-4 and magi-5 oracleConfig.json, restarting only those two")
	vfStopNodes(t, d, ctx, misconf)
	vfStartNodes(t, d, ctx, misconf)
	misconfAlive := vfF22WaitAlive(t, d, ctx, misconf, heightsBefore, 8*time.Minute)
	if !misconfAlive {
		t.Errorf("F22 SETUP: magi-4 and magi-5 did not resume processing within 8 minutes after the misconfigured restart; the fail-open observation below may be vacuous")
	}
	// Put the correct id back on DISK immediately. Nodes 1..3 never re-read the file,
	// so they keep the correct value; nodes 4 and 5 keep the empty value they loaded
	// at boot until they are restarted again in step 5. Restoring now means an
	// unplanned restart of any node cannot pick up the poisoned file.
	if err := d.SetOracleContractIDs(map[string]string{"BTC": cid}); err != nil {
		t.Errorf("F22 SETUP: could not restore the sysconfig BTC contract id on disk: %v", err)
	}

	// ---- 2. inject the rogue request on ALL five nodes ----
	rogue := sha256.Sum256([]byte("f22-attacker"))
	refuseBefore := map[int]int{}
	sessionBefore := map[int]int{}
	dispatchBefore := map[int]int{}
	for _, n := range nodes {
		refuseBefore[n], _ = vfF22CountLogLines(d, ctx, n, vfF13RefuseLog)
		sessionBefore[n], _ = vfF22CountLogLines(d, ctx, n, vfF22SessionLog, retiringKeyId)
		dispatchBefore[n], _ = vfF22CountLogLines(d, ctx, n, vfF22DispatchLog, retiringKeyId)
	}
	vfF13InsertRogueRequest(t, d, ctx, nodes, retiringKeyId, rogue[:])
	vfF13WaitSignTicks(t, d, ctx, signInterval, 3, 4*time.Minute)

	// ---- 3. F22-NOSIG ----
	msgHex := hex.EncodeToString(rogue[:])
	var signedOn []int
	for _, n := range nodes {
		if vfSignatureLanded(d, ctx, n, retiringKeyId, rogue[:]) {
			signedOn = append(signedOn, n)
		}
	}
	// "not signed" only means something if the nodes could actually see the row.
	rowVisible := 0
	for _, n := range nodes {
		reqs, err := d.GetTssRequests(ctx, n, retiringKeyId)
		if err != nil {
			continue
		}
		for _, r := range reqs {
			if r.Msg == msgHex {
				rowVisible++
				break
			}
		}
	}
	c.rec("F22-NOSIG", "a rogue digest for the retiring key is signed on no node while a minority runs with the gate off",
		len(signedOn) == 0 && rowVisible == len(nodes),
		fmt.Sprintf("digest=%s signedOnNodes=%v injectedRowVisibleOn=%d/5 (3 honest refusals leave 2 willing parties, the signing threshold needs 4 of 5)",
			msgHex, signedOn, rowVisible))

	// ---- 4. F22-GATE-OFF (INFO, ok=true: the observation is the product) ----
	refuseDelta := map[int]int{}
	sessionDelta := map[int]int{}
	dispatchDelta := map[int]int{}
	readOK := true
	for _, n := range nodes {
		rc, ok1 := vfF22CountLogLines(d, ctx, n, vfF13RefuseLog)
		sc, ok2 := vfF22CountLogLines(d, ctx, n, vfF22SessionLog, retiringKeyId)
		dc, ok3 := vfF22CountLogLines(d, ctx, n, vfF22DispatchLog, retiringKeyId)
		if !ok1 || !ok2 || !ok3 {
			readOK = false
			continue
		}
		refuseDelta[n] = rc - refuseBefore[n]
		sessionDelta[n] = sc - sessionBefore[n]
		dispatchDelta[n] = dc - dispatchBefore[n]
	}
	gatedRefusers := 0
	for _, n := range gated {
		if refuseDelta[n] >= 1 {
			gatedRefusers++
		}
	}
	misconfRefusals := 0
	misconfSessions := 0
	for _, n := range misconf {
		misconfRefusals += refuseDelta[n]
		misconfSessions += sessionDelta[n]
	}
	detail := fmt.Sprintf("INFO (not a pass): logsReadable=%v configuredNodes%v refusalDelta", readOK, gated)
	for _, n := range gated {
		detail += fmt.Sprintf(" magi-%d:+%d", n, refuseDelta[n])
	}
	detail += fmt.Sprintf(" (%d/3 refused); misconfiguredNodes%v", gatedRefusers, misconf)
	for _, n := range misconf {
		detail += fmt.Sprintf(" magi-%d: refusal+%d signSession+%d dispatcherStart+%d",
			n, refuseDelta[n], sessionDelta[n], dispatchDelta[n])
	}
	detail += fmt.Sprintf("; expected fail-open shape: 3 refusals, 0 refusals plus >0 sign sessions on the misconfigured pair (observed misconfiguredRefusals=%d misconfiguredSignSessions=%d, alive=%v)",
		misconfRefusals, misconfSessions, misconfAlive)
	c.rec("F22-GATE-OFF", "a node whose oracle config lacks the BTC contract id runs with the output-scoping gate silently off",
		true, detail)
	if misconfRefusals > 0 || misconfSessions == 0 {
		t.Logf("F22-GATE-OFF NOTE: the misconfigured pair did not show the expected fail-open shape (refusals=%d signSessions=%d). Either the empty BTC contract id did not reach magi-4/5 at boot, or they never reached a sign tick for %s. Read the raw counts above before drawing a conclusion.",
			misconfRefusals, misconfSessions, retiringKeyId)
	}

	// ---- 5. restore the configuration, then the mandatory positive control ----
	if err := d.WriteOracleConfigs(ctx); err != nil {
		t.Errorf("F22 RESTORE: rewriting the per-node oracle configs failed: %v", err)
	}
	if err := d.SetOracleContractIDs(map[string]string{"BTC": cid}); err != nil {
		t.Errorf("F22 RESTORE: restoring the sysconfig BTC contract id failed: %v", err)
	}
	restoreHeights := vfF22ProcessedHeights(t, d, ctx, misconf)
	restoreRefuse := map[int]int{}
	for _, n := range misconf {
		restoreRefuse[n], _ = vfF22CountLogLines(d, ctx, n, vfF13RefuseLog)
	}
	vfStopNodes(t, d, ctx, misconf)
	vfStartNodes(t, d, ctx, misconf)
	if !vfF22WaitAlive(t, d, ctx, misconf, restoreHeights, 8*time.Minute) {
		t.Errorf("F22 RESTORE: magi-4 and magi-5 did not resume processing within 8 minutes after the restoring restart")
	}

	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	migrateAndSettle(t, d, ctx, cid, retiringKeyId, primary1, backupPubKeyG)
	gen0After := gen0Before
	for i := 0; i < 12; i++ {
		gen0After = genUtxoCount(t, d, ctx, cid, 0)
		if gen0After == 0 {
			break
		}
		time.Sleep(15 * time.Second)
	}
	c.rec("F22-CONTROL", "positive control: with the configuration restored, a legitimate successor sweep for the same retiring key signs and settles",
		gen0After == 0,
		fmt.Sprintf("gen-0 UTXO count %d -> %d (0 means the retiring key signed the successor-paying sweep and it settled)", gen0Before, gen0After))

	// The rogue row is still unsigned, so every sign tick since the restore re-ran the
	// gate on magi-4 and magi-5. A non-zero delta here is the evidence that the
	// configuration, and nothing else, is what had turned their gate off.
	backOn := ""
	for _, n := range misconf {
		after, _ := vfF22CountLogLines(d, ctx, n, vfF13RefuseLog)
		backOn += fmt.Sprintf(" magi-%d:+%d", n, after-restoreRefuse[n])
	}
	stillUnsigned := true
	for _, n := range nodes {
		if vfSignatureLanded(d, ctx, n, retiringKeyId, rogue[:]) {
			stillUnsigned = false
		}
	}
	t.Logf("F22: refusal delta on the restored pair since the restoring restart:%s; rogue digest still unsigned on every node: %v", backOn, stillUnsigned)

	// ---- 6. F22-IDENT ----
	vfAssertContractIdentical(c, d, ctx, cid, nodes, 4*time.Minute, "F22-IDENT")
	c.summary("F22")
	t.Logf("F22 COMPLETE CONTRACT=%s", cid)
}

// vfF22WriteOracleConfigWithoutBtc rewrites ONLY the listed nodes' per-node oracle
// config (devnet-data/data-N/config/oracleConfig.json, the file WriteOracleConfigs
// produces) with the BTC entry removed, keeping any other enabled chain. This is the
// operator-visible half of the misconfiguration; on its own it only disables that
// node's BTC chain relay, because modules/oracle/config.go oracleConfig holds RPC
// details and no contract id. The half that actually turns the theft gate off is the
// empty ChainContracts BTC entry in the shared sysconfig, written by the caller.
func vfF22WriteOracleConfigWithoutBtc(t *testing.T, d *Devnet, nodes []int) {
	t.Helper()
	chains := map[string]chainRpcConfigJSON{}
	if d.cfg.EnableDashd {
		chains["DASH"] = chainRpcConfigJSON{
			RpcHost: d.DashdRPCHostPort(),
			RpcUser: "vsc-node-user",
			RpcPass: "vsc-node-pass",
		}
	}
	data, err := json.MarshalIndent(oracleConfigJSON{Chains: chains}, "", "  ")
	if err != nil {
		t.Fatalf("marshaling the BTC-less oracle config: %v", err)
	}
	for _, n := range nodes {
		rel := fmt.Sprintf("data-%d/config/oracleConfig.json", n)
		// Root-owned data dir: write through a root container (run 1 died on a
		// host-side os.WriteFile with "permission denied").
		if err := vfWriteNodeFileAsRoot(d.devnetDir, rel, data); err != nil {
			t.Fatalf("writing %s/%s: %v", d.devnetDir, rel, err)
		}
		t.Logf("F22: rewrote %s/%s without a BTC entry", d.devnetDir, rel)
	}
}

// vfF22ProcessedHeights snapshots each listed node's last processed block. A node's
// processed height only advances while its process is running and past config load,
// so it is the liveness instrument for a restart.
func vfF22ProcessedHeights(t *testing.T, d *Devnet, ctx context.Context, nodes []int) map[int]uint64 {
	t.Helper()
	out := map[int]uint64{}
	for _, n := range nodes {
		bh, err := d.getLastProcessedBlock(ctx, n)
		if err != nil {
			t.Logf("F22: could not read magi-%d processed height (%v), treating it as 0", n, err)
			bh = 0
		}
		out[n] = bh
	}
	return out
}

// vfF22WaitAlive waits until every listed node has processed a block beyond the
// snapshot taken before it was stopped. Because cmd/vsc-node/main.go loads the
// -sysconfig overrides at startup, well before block processing begins, a node that
// has advanced is a node that has already read the config file that was on disk when
// it booted.
func vfF22WaitAlive(t *testing.T, d *Devnet, ctx context.Context, nodes []int, from map[int]uint64, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		allUp := true
		detail := ""
		for _, n := range nodes {
			bh, err := d.getLastProcessedBlock(ctx, n)
			if err != nil || bh <= from[n] {
				allUp = false
			}
			detail += fmt.Sprintf(" magi-%d:%d(from %d,err=%v)", n, bh, from[n], err)
		}
		if allUp {
			t.Logf("F22: restarted nodes are processing again:%s", detail)
			return true
		}
		if time.Now().After(deadline) {
			t.Logf("F22: WARNING restarted nodes did not resume within %v:%s", timeout, detail)
			return false
		}
		select {
		case <-ctx.Done():
			t.Logf("F22: context done while waiting for the restarted nodes")
			return false
		case <-time.After(10 * time.Second):
		}
	}
}

// vfF22CountLogLines counts the LINES of a node's container log that contain every
// one of the substrings. Line based (not vfCountLogs' whole-buffer count) because the
// sign-session evidence is "this message AND this key id in the same line": the
// sessionId is "sign-<bh>-<idx>-<keyId>", so the key id and the message only appear
// together on the line belonging to that key's session. ok=false means the log could
// not be read at all, which must not be reported as a count of zero.
func vfF22CountLogLines(d *Devnet, ctx context.Context, node int, subs ...string) (int, bool) {
	logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", node))
	if err != nil {
		return 0, false
	}
	if len(subs) == 0 {
		return 0, true
	}
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		match := true
		for _, s := range subs {
			if !strings.Contains(line, s) {
				match = false
				break
			}
		}
		if match {
			n++
		}
	}
	return n, true
}
