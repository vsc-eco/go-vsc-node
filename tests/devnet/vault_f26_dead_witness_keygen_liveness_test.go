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
//
//  2. F26-FROZEN: after createKey, the gen-1 key never reaches active across many
//     rotate intervals; timeout/retry log lines accumulate; the generation stays Pending.
//
//  3. F26-SIGN-OK: an ordinary withdrawal from the ACTIVE gen-0 is fully signed by the
//     four live parties and accepted by Bitcoin (signing liveness is intact, so the
//     freeze is keygen-specific).
//
//  4. F26-ELECTION: two elections later the dead witness is still a committee member.
//
//  5. F26-DISCARD: discardPendingKey abandons the stuck generation (the state machine
//     has an exit), but a fresh createKey freezes exactly the same way (F26-REKEY-FROZEN).
//
//  6. F26-RECOVER: bringing the witness back lets the retried keygen complete.
//
//  7. F26-IDENT: all five nodes agree.
//
//     VAULT_F26_RUN=1 go test -v -run TestVaultF26DeadWitnessKeygenLiveness -timeout 95m ./tests/devnet/
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
	cfg := tssTestConfig()
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

	// ---- 2. start a keygen; it can never complete ----
	e0 := currentEpoch(t, d, ctx, 1, 2*time.Minute)
	timeouts0 := vfCountLogs(d, ctx, 1, "timeout result")
	retries0 := vfCountLogs(d, ctx, 1, "will retry at next rotate interval")
	if s := vstatus(t, d, ctx, 1, cid, "createKey", ""); !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: createKey status=%s, no keygen to freeze", s)
	}
	keyId1, gen1 := pendingKeyId()
	if keyId1 == "" {
		t.Fatalf("PRECONDITION FAILED: createKey accepted but no Pending generation is visible")
	}
	frozenFor := 7 * time.Minute
	became, status := f26WaitKeyActive(d, ctx, 1, keyId1, frozenFor)
	timeouts1 := vfCountLogs(d, ctx, 1, "timeout result")
	retries1 := vfCountLogs(d, ctx, 1, "will retry at next rotate interval")
	genStat := vfVaultStatusOn(d, ctx, 2, cid, gen1)
	c.rec("F26-FROZEN", "the keygen never completes while one committee member is permanently down (DKG needs every party, unlike signing)",
		!became && genStat == 0 && timeouts1 > timeouts0,
		fmt.Sprintf("keyId=%s status after %s=%q, gen-%d vault status=%d (0=Pending), magi-1 'timeout result' %d->%d, 'will retry' %d->%d",
			keyId1, frozenFor, status, gen1, genStat, timeouts0, timeouts1, retries0, retries1))

	// ---- 3. signing still works: a withdrawal from the active gen-0 ----
	dest, _ := d.bitcoinCli(ctx, "getnewaddress")
	spendsBefore := txSpendIds(t, d, ctx, cid)
	um := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, 5_000_000, dest))
	txU := ""
	for i := 0; i < 20 && isOK(um) && txU == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(spendsBefore, id) {
				txU = id
			}
		}
	}
	signDetail := fmt.Sprintf("unmap=%s txid=%s", um, txU)
	signOK := false
	if txU != "" {
		if sdU := waitSigningData(t, d, ctx, cid, txU); sdU != nil {
			rawU, signedU := "", false
			for attempt := 1; attempt <= 3 && !signedU; attempt++ {
				rawU, signedU = f25AwaitSignatures(t, d, ctx, cid+"-main", sdU)
			}
			if signedU {
				bc, berr := d.bitcoinCli(ctx, "sendrawtransaction", rawU)
				signOK = berr == nil
				signDetail += fmt.Sprintf(" signed_by_4_of_5=true broadcast=%s err=%v", bc, berr)
			} else {
				signDetail += " signed=false (no full witness within three sign intervals)"
			}
		} else {
			signDetail += " no signing data"
		}
	}
	c.rec("F26-SIGN-OK", "an ordinary withdrawal from the active generation is still signed (4 of 5 meets the threshold) and accepted by Bitcoin",
		signOK, signDetail)

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

	// ---- 5. the state machine has an exit, keygen liveness does not ----
	disc := vstatus(t, d, ctx, 1, cid, "discardPendingKey", "")
	time.Sleep(10 * time.Second)
	afterDisc := vfVaultStatusOn(d, ctx, 2, cid, gen1)
	c.rec("F26-DISCARD", "discardPendingKey abandons the stuck Pending generation",
		isOK(disc) && afterDisc != 0, fmt.Sprintf("discardPendingKey=%s gen-%d status after=%d (want not 0)", disc, gen1, afterDisc))

	rekey := vstatus(t, d, ctx, 1, cid, "createKey", "")
	keyId2, gen2 := pendingKeyId()
	became2, status2 := false, "(no pending gen)"
	if keyId2 != "" {
		became2, status2 = f26WaitKeyActive(d, ctx, 1, keyId2, 4*time.Minute)
	}
	c.rec("F26-REKEY-FROZEN", "a fresh createKey after the discard freezes the same way while the witness is still down",
		isOK(rekey) && keyId2 != "" && !became2,
		fmt.Sprintf("createKey=%s keyId=%s gen=%d status after 4m=%q", rekey, keyId2, gen2, status2))

	// ---- 6. only the witness's return unblocks it ----
	vfStartNodes(t, d, ctx, []int{dead})
	recoverKey := keyId2
	if recoverKey == "" {
		recoverKey = keyId1
	}
	kd, err := d.WaitForTssKey(ctx, 2, bson.M{"id": recoverKey, "status": "active"}, 14*time.Minute)
	recDetail := fmt.Sprintf("keyId=%s", recoverKey)
	if err == nil && kd != nil {
		recDetail += fmt.Sprintf(" active epoch=%d pubkey=%s", kd.Epoch, f3TruncHex(kd.PublicKey))
	} else {
		recDetail += fmt.Sprintf(" err=%v", err)
	}
	c.rec("F26-RECOVER", "once the witness is back the retried keygen completes",
		err == nil && kd != nil, recDetail)

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
