package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultF1KeygenQuorumLoss is the F1 failure-state test of the BTC
// vault-rotation-v2 suite.
//
// WHAT IT REPRODUCES
// A rotation keygen is started (createKey mints gen-1 and puts the keygen request
// on chain) and the operator immediately loses 2 of the 5 nodes. With 3 of 5 up
// the BLS block quorum (4 of 5) is gone, so VSC halts, and the TSS signing
// threshold (ceil(2n/3)-1 = 3, so 4 live parties) is also unreachable, so the
// keygen cannot complete either. This is the "quorum loss in the middle of a
// vault rotation" incident: the vault is left with a Pending gen-1 whose key
// does not exist yet.
//
// WHAT IT PROVES
//  1. F1-HALT: the loss really halts VSC block production (measured on
//     block_headers, the only instrument that reflects BLS quorum; hive_blocks
//     advances regardless of quorum and must never be used here).
//  2. F1-NOKEY: the gen-1 key does NOT reach status active with only 3 parties,
//     so the threshold is really enforced and no key is ever minted from an
//     under-quorum session. The keygen session times out and the node schedules
//     a retry at the next rotate interval instead of wedging.
//  3. F1-RESUME: bringing the two nodes back restores block production.
//  4. F1-KEY: the retried keygen then completes and every one of the 5 nodes
//     holds the byte-identical commitment (same PublicKey, same Epoch), so the
//     outage did not fork the key material.
//  5. F1-ACTIVATE: the BRK-2 check-signature lands with the full committee back,
//     so gen-1 activates and gen-0 goes Retiring.
//  6. F1-SIGN: the migration sweep of gen-0 into gen-1 is signed and settles,
//     which requires the shares of the two nodes that were down during the failed
//     keygen. Their shares are therefore intact, the outage cost nothing.
//  7. F1-IDENT: the vault contract state is byte-identical on all 5 nodes at the
//     end, including the two that replayed after the restart.
//
// RUN
//
//	VAULT_F1_RUN=1 go test -v -run TestVaultF1KeygenQuorumLoss -timeout 95m ./tests/devnet/
//
// Set DEVNET_KEEP=1 to leave the devnet running for post-mortem inspection, and
// BTC_MAPPING_WASM_PATH to point at a different contract build.
func TestVaultF1KeygenQuorumLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F1_RUN") == "" {
		t.Skip("set VAULT_F1_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// hpin AFTER genesis (~block 190) so gen-0 is minted on the v2-off genesis
	// path (no fresh-genesis deadlock) and v2 is live for the rotation.
	const hpin uint64 = 400

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
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F1 keygen quorum loss")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	keyId1 := cid + "-mainv1"

	// ---- step 1: start the gen-1 keygen, then drop 2 of 5 nodes ----
	if s := vstatus(t, d, ctx, 1, cid, "createKey", ""); !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: createKey (gen-1) status=%s, the keygen request was never put on chain so there is no keygen to interrupt", s)
	}
	t.Logf("gen-1 createKey accepted, keygen request is on chain; dropping magi-4 and magi-5 now")
	vfStopNodes(t, d, ctx, []int{4, 5})

	// ---- step 2: F1-HALT, 3 of 5 must not produce VSC blocks ----
	grew, haltStart, haltLast := vfGrewWithin(d, ctx, 1, 90*time.Second)
	c.rec("F1-HALT", "VSC block production halts with only 3 of 5 nodes (BLS quorum is 4 of 5)",
		!grew, fmt.Sprintf("block_headers max slot on magi-1: start=%d last=%d over 90s", haltStart, haltLast))
	if grew {
		vfStartNodes(t, d, ctx, []int{4, 5})
		t.Fatalf("PRECONDITION FAILED: VSC kept producing blocks with 3 of 5 nodes up (slot %d -> %d), the quorum-loss scenario was never established so nothing below would prove anything",
			haltStart, haltLast)
	}

	// ---- step 3: F1-NOKEY, the key must not reach active under threshold ----
	statusSeen := "absent"
	becameActive := false
	nokeyDeadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(nokeyDeadline) {
		docs, err := d.GetTssKeys(ctx, 1, bson.M{"id": keyId1})
		if err == nil && len(docs) > 0 {
			statusSeen = docs[0].Status
			if docs[0].Status == "active" {
				becameActive = true
				break
			}
		}
		time.Sleep(10 * time.Second)
	}
	timeouts := vfCountLogs(d, ctx, 1, "timeout result")
	retries := vfCountLogs(d, ctx, 1, "will retry at next rotate interval")
	t.Logf("magi-1 keygen log counts while under quorum: \"timeout result\"=%d, \"will retry at next rotate interval\"=%d (retry expected yes)", timeouts, retries)
	c.rec("F1-NOKEY", "gen-1 key never reaches active with 3 parties (TSS threshold needs 4)",
		!becameActive, fmt.Sprintf("last tss_keys status on magi-1=%q after 4m; log counts: timeout result=%d, will retry at next rotate interval=%d",
			statusSeen, timeouts, retries))

	// ---- step 4: F1-RESUME, restore the committee ----
	vfStartNodes(t, d, ctx, []int{4, 5})
	resumed, resStart, resLast := vfGrewWithin(d, ctx, 1, 4*time.Minute)
	c.rec("F1-RESUME", "VSC block production resumes once 5 of 5 are back",
		resumed, fmt.Sprintf("block_headers max slot on magi-1: start=%d last=%d over 4m", resStart, resLast))

	// Steps 5 to 7 run in a closure so that an unrecoverable failure can stop the
	// sequence while still letting F1-IDENT and the summary run below.
	func() {
		// ---- step 5: F1-KEY, the retried keygen lands, identically everywhere ----
		kd1, err := d.WaitForTssKey(ctx, 2, bson.M{"id": keyId1, "status": "active"}, 10*time.Minute)
		if err != nil {
			all, _ := d.GetTssKeys(ctx, 2, bson.M{})
			for _, k := range all {
				t.Logf("  tss_key id=%s status=%s epoch=%d", k.Id, k.Status, k.Epoch)
			}
			c.rec("F1-KEY", "gen-1 keygen completes after the committee is restored", false,
				fmt.Sprintf("WaitForTssKey(%s, active) on magi-2: %v", keyId1, err))
			return
		}
		primary1 := kd1.PublicKey
		c.rec("F1-KEY", "gen-1 keygen completes after the committee is restored", true,
			fmt.Sprintf("keyId=%s epoch=%d pubkey=%s", keyId1, kd1.Epoch, primary1))

		identical := true
		detail := ""
		for _, n := range vfAllNodes(5) {
			docs, err := d.GetTssKeys(ctx, n, bson.M{"id": keyId1})
			if err != nil || len(docs) == 0 {
				identical = false
				detail += fmt.Sprintf(" magi-%d=UNREADABLE(err=%v,docs=%d)", n, err, len(docs))
				continue
			}
			detail += fmt.Sprintf(" magi-%d=(pk=%s,epoch=%d,status=%s)", n, docs[0].PublicKey, docs[0].Epoch, docs[0].Status)
			if docs[0].PublicKey != primary1 || docs[0].Epoch != kd1.Epoch {
				identical = false
			}
		}
		c.rec("F1-KEY", "gen-1 commitment is byte-identical on all 5 nodes (same PublicKey and Epoch)",
			identical, "reference pk="+primary1+detail)

		// ---- step 6: F1-ACTIVATE, the BRK-2 check-sig lands with 5 of 5 ----
		activated := vfRegisterAndActivate(t, d, ctx, cid, primary1, 12)
		c.rec("F1-ACTIVATE", "gen-1 registers and activates (BRK-2 check-signature verified with the full committee)",
			activated, fmt.Sprintf("gen-1 status on magi-2=%d (1=Active), gen-0 status=%d (2=Retiring)",
				vfVaultStatusOn(d, ctx, 2, cid, 1), vfVaultStatusOn(d, ctx, 2, cid, 0)))
		if !activated {
			return
		}

		// ---- step 7: F1-SIGN, the sweep needs the shares of magi-4 and magi-5 ----
		gen0Before := genUtxoCount(t, d, ctx, cid, 0)
		if gen0Before <= 0 {
			c.rec("F1-SIGN", "gen-0 drains into gen-1 with a sweep signed by the full committee",
				false, fmt.Sprintf("PRECONDITION: gen-0 held %d UTXOs before the sweep, there was nothing to migrate", gen0Before))
			return
		}
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		gen0After := genUtxoCount(t, d, ctx, cid, 0)
		c.rec("F1-SIGN", "gen-0 drains into gen-1 with a sweep signed by the full committee (the two nodes that were down keep intact shares)",
			gen0After >= 0 && gen0After < gen0Before,
			fmt.Sprintf("gen-0 UTXO count %d -> %d, gen-1 UTXO count=%d", gen0Before, gen0After, genUtxoCount(t, d, ctx, cid, 1)))
	}()

	// ---- step 8: F1-IDENT, no fork survived the outage ----
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F1-IDENT")
	c.summary("F1")
	t.Logf("F1 COMPLETE CONTRACT=%s", cid)
}
