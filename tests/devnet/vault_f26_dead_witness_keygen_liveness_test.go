package devnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"vsc-node/lib/btcvault"
)

// TestVaultF26DeadWitnessKeygenLiveness is failure-state F26 of the BTC vault-rotation-v2
// suite: ONE witness goes permanently dark while still enabled.
//
// WHY THIS IS ITS OWN CASE
// F1 drops two nodes and loses block quorum; F3 crashes a node briefly. Neither asks
// the operational question: what if one of the five simply never comes back (hardware
// gone, operator vanished) but its on-chain announcement still says enabled=true?
//   - Block production and BLS quorum survive (4 of 5).
//   - TSS SIGNING survives: the threshold is ceil(2n/3)-1 = 3, so 4 live parties sign.
//   - TSS KEYGEN does NOT: the DKG is started for every committee member
//     (modules/tss/dispatcher.go KeyGenDispatcher.Start builds btss parameters with
//     pl = len(participants) and has no readiness gate, unlike reshare), so a missing
//     party stalls every round until the session times out and the node "will retry at
//     next rotate interval", forever.
//   - Elections never drop it: election-proposer.go:372 takes
//     GetWitnessesAtBlockHeight(blk, EnabledOnly()) and the "witness active score" is a
//     TODO (election-proposer.go:127,388). Only the dead witness's OWN key can flip
//     enabled=false (announcements.go:332), so no one else can unblock the rotation.
//
// CASES
//
//  1. F26-BLOCKS: 4 of 5 keep producing VSC blocks (block_headers instrument).
//  2. F26-KEYGEN-COMPLETES: the gen-1 keygen COMPLETES with one witness permanently
//     down. VSC keygen/reshare is threshold-based (t+1 = 4 of 5), not all-parties: the
//     dispatcher runs on the readiness-filtered subset with the original-size threshold
//     (dispatcher.go:180) and a tss_key is marked active only on a BLS-threshold-verified
//     keygen commitment (state_engine.go:1651). So n-(t+1)=1 missing party is tolerated.
//  3. F26-ACTIVATES: the dead-witness key is USABLE — it activates (BRK-2 check-signature
//     signed by the 4 live parties).
//  4. F26-MIGRATES: the dead-witness key SIGNS the gen-0 -> gen-1 migration sweep, so the
//     rotation actually completes with one witness down.
//  5. F26-ELECTION: the dead-but-enabled witness stays a committee member (elections have
//     no liveness score; only its own key could remove it) — the residual VR2-08 concern:
//     it erodes the failure margin, but does not freeze rotation.
//  6. F26-RECOVER: the returned witness rejoins and catches up (full 5-of-5 restored).
//  7. F26-IDENT: all five nodes agree.
//
// This REPLACES the earlier "the keygen freezes with one dead witness" premise, which was
// wrong: run 1 saw the key reach active with one node down (the gen stayed Pending only
// because of the fast cadence's VR2-09 lock). A two-down boundary is not testable here —
// for n=5 the keygen threshold (t+1=4) equals the BLS block quorum (4 of 5), so two down
// halts the chain first (see F1-HALT), not the keygen specifically.
//
//	VAULT_F26_RUN=1 go test -v -run TestVaultF26DeadWitnessKeygenLiveness -timeout 95m ./tests/devnet/
func TestVaultF26DeadWitnessKeygenLiveness(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F26_RUN") == "" {
		t.Skip("set VAULT_F26_RUN=1")
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

	const hpin = 400
	// Slow cadence: F26 activates and migrates the dead-witness key, and on the 20-block
	// tssTestConfig cadence VR2-09 would block that activation independently of the dead
	// witness (confounding the measurement). The 60-block cadence gives the check-sig a
	// window (H-17).
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)

	c := &vfCase{t: t}

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F26 dead witness keygen liveness")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	const dead = 5
	deadAccount := d.witnessAccount(dead)

	finish := func() {
		vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F26-IDENT")
		c.summary("F26")
		t.Logf("F26 COMPLETE CONTRACT=%s", cid)
	}

	// pendingKeyId finds the keyId of whichever generation is currently Pending.
	pendingKeyId := func() (string, uint32) {
		for g := uint32(1); g <= 4; g++ {
			if vfVaultStatusOn(d, ctx, 2, cid, g) == 0 {
				return cid + "-" + btcvault.VaultKeyName(g), g
			}
		}
		return "", 0
	}

	// ---- 1. the witness dies for good ----
	vfStopNodes(t, d, ctx, []int{dead})
	grew, gStart, gLast := vfGrewWithin(d, ctx, 1, 90*time.Second)
	c.rec("F26-BLOCKS", "VSC keeps producing blocks with 4 of 5 (BLS quorum is 4 of 5)",
		grew, fmt.Sprintf("block_headers max slot on magi-1: start=%d last=%d over 90s", gStart, gLast))
	if !grew {
		t.Errorf("block production stopped with only one node down; the keygen freeze below cannot be separated from a chain halt")
		finish()
		return
	}

	// ---- 2. a keygen COMPLETES with one witness down (threshold t+1 = 4 of 5) ----
	e0 := currentEpoch(t, d, ctx, 1, 2*time.Minute)
	ckStatus := vstatus(t, d, ctx, 1, cid, "createKey", "")
	keyId1, gen1 := pendingKeyId()
	for i := 0; i < 24 && keyId1 == ""; i++ {
		time.Sleep(10 * time.Second)
		keyId1, gen1 = pendingKeyId()
	}
	if keyId1 == "" {
		t.Fatalf("PRECONDITION FAILED: createKey status=%s and no Pending generation visible after 4 min", ckStatus)
	}
	t.Logf("createKey status=%s; pending gen-%d keyId=%s", ckStatus, gen1, keyId1)
	became, kstatus := f26WaitKeyActive(d, ctx, 1, keyId1, 12*time.Minute)
	primary1 := ""
	if docs, e := d.GetTssKeys(ctx, 2, bson.M{"id": keyId1}); e == nil && len(docs) > 0 {
		primary1 = docs[0].PublicKey
	}
	c.rec("F26-KEYGEN-COMPLETES", "the gen-1 keygen COMPLETES with one committee member permanently down (threshold t+1 = 4 of 5; DKG is not all-parties)",
		became && primary1 != "",
		fmt.Sprintf("keyId=%s status after 12m=%q pubkey=%s (magi-%d down the whole time)", keyId1, kstatus, f3TruncHex(primary1), dead))

	// ---- 3. the dead-witness key ACTIVATES (BRK-2 check-sig signed by 4 of 5) ----
	activated := false
	if became && primary1 != "" {
		vfWaitPreparams(t, d, ctx, 12*time.Minute) // VR2-09: let the post-DKG pre-parameter regen finish
		activated = vfRegisterAndActivate(t, d, ctx, cid, primary1, 20)
	}
	gen1Stat := vfVaultStatusOn(d, ctx, 2, cid, gen1)
	c.rec("F26-ACTIVATES", "the key generated with one witness down is USABLE: it activates (BRK-2 check-signature signed by the 4 live parties)",
		activated && gen1Stat == 1,
		fmt.Sprintf("gen-%d vault status=%d (1=Active) with magi-%d down", gen1, gen1Stat, dead))

	// ---- 4. the dead-witness key SIGNS a migration sweep (gen-0 drains into gen-1) ----
	if activated {
		gen0Before := genUtxoCount(t, d, ctx, cid, 0)
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		gen0After := genUtxoCount(t, d, ctx, cid, 0)
		c.rec("F26-MIGRATES", "the dead-witness key SIGNS the gen-0 -> gen-1 migration sweep (4 of 5 sign), so the rotation completes with one witness down",
			gen0Before > 0 && gen0After < gen0Before,
			fmt.Sprintf("gen-0 UTXO %d -> %d with magi-%d down", gen0Before, gen0After, dead))
	} else {
		c.rec("F26-MIGRATES", "the dead-witness key SIGNS the gen-0 -> gen-1 migration sweep", false, "not reached: gen-1 did not activate")
	}
	// ---- 4. elections keep the dead witness ----
	elDetail := ""
	stillElected := false
	if err := d.waitForElectionEpoch(ctx, 1, e0+2, 8*time.Minute); err != nil {
		elDetail = fmt.Sprintf("epoch %d never arrived: %v", e0+2, err)
	} else {
		members, merr := d.GetElectionMembers(ctx, 1, e0+2)
		for _, m := range members {
			if m == deadAccount {
				stillElected = true
			}
		}
		elDetail = fmt.Sprintf("epoch %d (started at %d) members=%v dead=%s err=%v", e0+2, e0, members, deadAccount, merr)
	}
	c.rec("F26-ELECTION", "two elections later the dead-but-enabled witness is still a committee member (elections have no liveness score)",
		stillElected, elDetail)

	// ---- 6. the dead witness returns, rejoins and catches up (5-of-5 margin restored) ----
	vfStartNodes(t, d, ctx, []int{dead})
	target, terr := d.getLastProcessedBlock(ctx, 1)
	caughtUp := false
	if terr == nil {
		caughtUp = vfWaitProcessed(t, d, ctx, dead, target, 10*time.Minute)
	}
	bhDead, _ := d.getLastProcessedBlock(ctx, dead)
	c.rec("F26-RECOVER", "the returned witness rejoins and catches up to the fleet (full 5-of-5 margin restored; the rotation had already completed without it)",
		caughtUp, fmt.Sprintf("target(magi-1)=%d magi-%d processed=%d within 10m (err=%v)", target, dead, bhDead, terr))

	finish()
}

// f26WaitKeyActive polls tss_keys on node n for keyId to reach status active, returning
// (reached, last status seen).
func f26WaitKeyActive(d *Devnet, ctx context.Context, node int, keyId string, within time.Duration) (bool, string) {
	deadline := time.Now().Add(within)
	last := "absent"
	for time.Now().Before(deadline) {
		docs, err := d.GetTssKeys(ctx, node, bson.M{"id": keyId})
		if err == nil && len(docs) > 0 {
			last = docs[0].Status
			if strings.EqualFold(last, "active") {
				return true, last
			}
		}
		time.Sleep(10 * time.Second)
	}
	return false, last
}
