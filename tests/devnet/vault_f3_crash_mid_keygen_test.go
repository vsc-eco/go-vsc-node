package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultF3CrashMidKeygen proves that a node which cold-restarts right before a
// keygen (so its pre-params pool is EMPTY and KeyGenDispatcher.Start blocks on
// `preParams := <-dispatcher.tssMgr.preParams`, modules/tss/dispatcher.go:1731) and a
// node which crashes in the MIDDLE of a keygen do not corrupt the key, do not panic,
// and can still sign afterwards.
//
// Why the two halves are different failures:
//   - Cold pool (step 1): magi-3 is restarted and the gen-1 createKey fires
//     immediately. GeneratePreParams (modules/tss/tss.go:372) has to find fresh 1024
//     bit safe primes before magi-3 can join, which on a loaded box takes far longer
//     than one keygen session. The other 4 parties are exactly the TSS threshold
//     (ceil(2n/3)-1 = 3, so 4 of 5 must be live), so gen-1 must still complete; if
//     magi-3 stalls the session the others time it out and retry at the next rotate
//     interval (20 blocks on tssTestConfig).
//   - Crash mid-session (step 3): magi-3 is killed 5s after the gen-2 createKey and
//     brought back 20s later, i.e. while the DKG rounds are in flight. The invariant
//     under test is that its persisted share is either absent or CORRECT, never a
//     half-written share that silently produces a wrong signature later.
//
// Only ONE node is ever down, so BLS block quorum (4 of 5) and the TSS threshold both
// hold throughout: this test is about share integrity, not about halts.
//
// NN#3 in the contract refuses createKey while a superseded generation still holds
// funds, so gen-0 is drained to zero UTXOs before the gen-2 createKey in step 3.
//
//	VAULT_F3_RUN=1 BTC_MAPPING_WASM_PATH=... go test -v -run TestVaultF3CrashMidKeygen -timeout 95m ./tests/devnet/
func TestVaultF3CrashMidKeygen(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F3_RUN") == "" {
		t.Skip("set VAULT_F3_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("BTC_MAPPING_WASM_PATH (%s): %v", wasm, err)
	}

	const hpin = 400 // v2 activation AFTER genesis, so gen-0 mints on the v2-off path
	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)

	c := &vfCase{t: t}

	// Setup: deploy, seed headers, wire the oracle, mint + register gen-0, fund it.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "F3 crash mid-keygen")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	mainv0 := cid + "-main"
	mainv1 := cid + "-mainv1"
	mainv2 := cid + "-mainv2"

	// finish runs the mandatory cross-node identity check and the summary line. Every
	// early return goes through it so a failed case still produces the fork verdict.
	finish := func() {
		vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F3-IDENT")
		c.summary("F3")
	}

	// -----------------------------------------------------------------
	// Step 1: cold-restart magi-3, then fire the gen-1 keygen immediately.
	// -----------------------------------------------------------------
	vfStopNodes(t, d, ctx, []int{3})
	vfStartNodes(t, d, ctx, []int{3})
	ck1 := vstatus(t, d, ctx, 1, cid, "createKey", "")
	t.Logf("gen-1 createKey status=%s (issued immediately after the magi-3 cold restart)", ck1)

	// -----------------------------------------------------------------
	// Step 2: F3-KEY. The remaining 4 parties must still produce gen-1.
	// -----------------------------------------------------------------
	kd1, err1 := d.WaitForTssKey(ctx, 1, bson.M{"id": mainv1, "status": "active"}, 10*time.Minute)
	timeouts1 := vfCountLogs(d, ctx, 1, "timeout result")
	retries1 := vfCountLogs(d, ctx, 1, "will retry at next rotate interval")
	_, needPre := vfLogsContainAny(d, ctx, 3, "need to generate preparams")
	_, gotPre := vfLogsContainAny(d, ctx, 3, "preparams generated successfully")
	key1Detail := fmt.Sprintf("createKey=%s magi-1 'timeout result'=%d 'will retry at next rotate interval'=%d, magi-3 need-preparams=%v preparams-generated=%v",
		ck1, timeouts1, retries1, needPre, gotPre)
	if err1 != nil || kd1 == nil {
		c.rec("F3-KEY", "gen-1 keygen completes although magi-3 restarted with an EMPTY pre-params pool", false,
			fmt.Sprintf("%s | WaitForTssKey(%s active) err=%v", key1Detail, mainv1, err1))
		finish()
		return
	}
	primary1 := kd1.PublicKey
	c.rec("F3-KEY", "gen-1 keygen completes although magi-3 restarted with an EMPTY pre-params pool", true,
		fmt.Sprintf("%s | pubkey=%s epoch=%d", key1Detail, f3TruncHex(primary1), kd1.Epoch))

	// -----------------------------------------------------------------
	// Step 3: activate gen-1, drain gen-0 (NN#3 precondition for the next
	// createKey), then crash magi-3 in the middle of the gen-2 keygen.
	// -----------------------------------------------------------------
	vfWaitPreparams(t, d, ctx, 12*time.Minute) // VR2-09: let the post-DKG pre-parameter regeneration finish first
	if !vfRegisterAndActivate(t, d, ctx, cid, primary1, 20) {
		t.Errorf("gen-1 never activated (BRK-2 check-signature never admitted); cannot reach the gen-2 keygen")
		finish()
		return
	}
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	for i := 0; i < 6 && genUtxoCount(t, d, ctx, cid, 0) != 0; i++ {
		migrateAndSettle(t, d, ctx, cid, mainv0, primary1, backupPubKeyG)
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	}
	gen0Left := genUtxoCount(t, d, ctx, cid, 0)
	if gen0Left != 0 {
		t.Errorf("gen-0 still holds %d UTXO(s) after the drain loop; NN#3 refuses createKey while a superseded gen holds funds, so the gen-2 keygen below cannot start", gen0Left)
		finish()
		return
	}
	t.Logf("gen-0 fully drained (0 UTXOs), NN#3 now admits the gen-2 createKey")

	ck2 := vstatus(t, d, ctx, 1, cid, "createKey", "")
	t.Logf("gen-2 createKey status=%s", ck2)
	time.Sleep(5 * time.Second)
	vfStopNodes(t, d, ctx, []int{3})
	time.Sleep(20 * time.Second)
	vfStartNodes(t, d, ctx, []int{3})

	// -----------------------------------------------------------------
	// Step 4: F3-KEY2, F3-NOPANIC, F3-SHARE.
	// -----------------------------------------------------------------
	kd2, err2 := d.WaitForTssKey(ctx, 1, bson.M{"id": mainv2, "status": "active"}, 10*time.Minute)
	timeouts2 := vfCountLogs(d, ctx, 1, "timeout result")
	retries2 := vfCountLogs(d, ctx, 1, "will retry at next rotate interval")
	key2Detail := fmt.Sprintf("createKey=%s magi-1 'timeout result'=%d (was %d) 'will retry'=%d (was %d)",
		ck2, timeouts2, timeouts1, retries2, retries1)
	key2OK := err2 == nil && kd2 != nil
	if key2OK {
		key2Detail += fmt.Sprintf(" | pubkey=%s epoch=%d", f3TruncHex(kd2.PublicKey), kd2.Epoch)
	} else {
		key2Detail += fmt.Sprintf(" | WaitForTssKey(%s active) err=%v", mainv2, err2)
	}
	c.rec("F3-KEY2", "gen-2 keygen completes although magi-3 was killed and restarted mid-session", key2OK, key2Detail)

	// F3-NOPANIC. vfLogsContainAny returns false when the log cannot be read at all,
	// so count the lines too: a zero-line log would make the check vacuous.
	logLines := vfCountLogs(d, ctx, 3, "\n")
	panicHit, panicked := vfLogsContainAny(d, ctx, 3, "panic:", "fatal error:")
	c.rec("F3-NOPANIC", "magi-3 never panicked across the cold-pool keygen and the mid-keygen crash",
		!panicked && logLines > 0,
		fmt.Sprintf("hit=%q logLines=%d (0 lines means the log was unreadable and the check would prove nothing)", panicHit, logLines))

	if !key2OK {
		finish()
		return
	}
	primary2 := kd2.PublicKey

	// F3-SHARE: gen-2 activates, every node holds the SAME gen-2 share, and the fleet
	// (magi-3 included, back up and holding an active mainv2 row) signs the gen-1
	// sweep with it. A half-written share on magi-3 would show up as a divergent
	// public key here, or as a sweep that never gathers its signatures.
	vfWaitPreparams(t, d, ctx, 12*time.Minute) // VR2-09: let the post-DKG pre-parameter regeneration finish first
	activated2 := vfRegisterAndActivate(t, d, ctx, cid, primary2, 20)
	// Each node flips the row to active when IT ingests the commitment's Hive block, so
	// a lagging node is polled for up to 3 minutes before being called divergent (F1 run
	// 1 recorded a false divergence from a single early read).
	sameKey := false
	rows := ""
	for attempt := 0; attempt < 18 && !sameKey; attempt++ {
		sameKey = true
		rows = ""
		for _, n := range vfAllNodes(5) {
			docs, e := d.GetTssKeys(ctx, n, bson.M{"id": mainv2})
			if e != nil || len(docs) == 0 {
				sameKey = false
				rows += fmt.Sprintf(" magi-%d=<absent err=%v>", n, e)
				continue
			}
			rows += fmt.Sprintf(" magi-%d=%s/e%d/%s", n, f3TruncHex(docs[0].PublicKey), docs[0].Epoch, docs[0].Status)
			if docs[0].PublicKey != primary2 || docs[0].Epoch != kd2.Epoch {
				sameKey = false
			}
		}
		if !sameKey {
			time.Sleep(10 * time.Second)
		}
	}

	gen1Left := -1
	if activated2 {
		fundFeeReserve(t, d, ctx, cid, primary2, backupPubKeyG, 10_000_000)
		for i := 0; i < 6 && genUtxoCount(t, d, ctx, cid, 1) != 0; i++ {
			vfDumpRegistry(t, d, ctx, 2, cid, fmt.Sprintf("before gen-1 drain tranche %d", i+1))
			migrateAndSettle(t, d, ctx, cid, mainv1, primary2, backupPubKeyG)
			fundFeeReserve(t, d, ctx, cid, primary2, backupPubKeyG, 10_000_000)
		}
		gen1Left = genUtxoCount(t, d, ctx, cid, 1)
	}
	c.rec("F3-SHARE", "gen-2 share identical on all 5 nodes and the crashed node's fleet signs the gen-1 sweep with it",
		activated2 && sameKey && gen1Left == 0,
		fmt.Sprintf("activated=%v sameKey=%v gen-1 UTXOs left=%d (want 0) |%s", activated2, sameKey, gen1Left, rows))

	// -----------------------------------------------------------------
	// Step 5: F3-IDENT across all 5 nodes, all of them up.
	// -----------------------------------------------------------------
	finish()
	t.Logf("F3 COMPLETE CONTRACT=%s", cid)
}

// f3TruncHex shortens a hex string for log lines without hiding a mismatch (the full
// values are compared, only the printed form is cut).
func f3TruncHex(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + ".."
}
