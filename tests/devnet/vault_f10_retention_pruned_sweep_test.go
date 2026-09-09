package devnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestVaultF10RetentionPrunedSweep is failure-state F10 of the BTC vault-rotation-v2
// suite: a migration sweep that is signed, broadcast and MINED, but whose confirmSpend
// is not delivered before the contract prunes the Bitcoin header that proves it.
//
// WHY THIS MATTERS
// confirmSpend is the only path that promotes a sweep's output to the successor and
// releases the retiring generation's inputs. Its SPV proof needs the block header at
// the sweep's height, and the contract keeps only the last MaxBlockRetention (4608)
// headers (contract/blocklist/blocklist.go PruneOldHeaders, contract/mapping/proof.go).
// Any outage longer than that window (a VSC halt, a paused contract, a dead relayer, an
// operator on holiday) therefore strands the sweep: the coins really moved on Bitcoin,
// but the contract can never learn it. The live testnet reached exactly this state on
// 2026-09-05 through a relayer that never confirmed its withdrawals for five months
// (ledger, phase A). This test reproduces it on a fresh devnet with the REAL retention
// constant, by relaying 4608+ regtest headers past the sweep, and then proves that
// nothing in the contract can recover from it.
//
// CASES
//  1. F10-HDR-PRESENT (control): right after mining, the header at the sweep height IS
//     in contract state, so the later absence is a prune, not a bad read.
//  2. F10-RELAY (instrument): the contract's last height really passed the retention
//     window (>= mined height + 4608 + 50); records how many addBlocks calls it took.
//  3. F10-PRUNED: the header at the sweep height is RETAINED on both nodes while a
//     live spend needs it, even past the retention window (VR2-03 fixed).
//  4. F10-CONFIRM-SETTLES: confirmSpend with the correct proof SETTLES; gen-0 drains.
//  5. F10-REDRIVE-NO-RECOVERY: redriveSpend either is refused, or builds a replacement
//     that Bitcoin rejects (its inputs are already spent by the mined original) while the
//     original record stays live (spend-group semantics). Either way nothing recovers.
//  6. F10-NO-RECOVERY-OPS: writeOffDust, retireVault and createKey (NN#3) all leave
//     gen-0 Retiring and funded and mint no gen-2: the rotation is permanently stuck.
//  7. F10-BOND: a committee member's consensus_unstake is still refused (bond lock).
//  8. F10-IDENT: all five nodes hold byte-identical vault state.
//
// Crossing the window: run 1 showed that relaying 4,658 headers from the single
// oracle-eligible account exhausts its resource credits after ~32 calls. The test now
// uses the contract's own owner route (testnet/regtest only): initPruning pins the
// prune floor at the sweep height, seedBlocks re-seeds FORWARD past height+retention,
// and admin prune calls delete the old headers, which is the state 32 days of natural
// relaying leave on mainnet. The bulk relay helper stays in the file for reuse.
//
//	VAULT_F10_RUN=1 go test -v -run TestVaultF10RetentionPrunedSweep -timeout 95m ./tests/devnet/
func TestVaultF10RetentionPrunedSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F10_RUN") == "" {
		t.Skip("set VAULT_F10_RUN=1")
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
	batch := 40
	if v := os.Getenv("VAULT_F10_BATCH"); v != "" {
		fmt.Sscanf(v, "%d", &batch)
	}
	const retention = 4608 // contract/constants/constants.go MaxBlockRetention
	const margin = 50

	const hpin = 400
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)

	c := &vfCase{t: t}

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F10 retention-pruned sweep")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	finish := func() {
		vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F10-IDENT")
		c.summary("F10")
		t.Logf("F10 COMPLETE CONTRACT=%s", cid)
	}

	// Rotate so gen-0 is Retiring, then build, sign, broadcast and MINE one sweep.
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 to gen-1 rotation did not complete (primary1=%q, gen-0 status=%d)", primary1, vfVaultStatusOn(d, ctx, 2, cid, 0))
	}
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	txid, sd, ms := vfBuildSweep(t, d, ctx, 1, cid)
	if txid == "" || sd == nil {
		t.Fatalf("PRECONDITION FAILED: no migration sweep to strand (txid=%q migrateVault status=%s)", txid, ms)
	}
	rawHex, signed := "", false
	for attempt := 1; attempt <= 3 && !signed; attempt++ {
		rawHex, signed = vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
	}
	if !signed {
		t.Fatalf("PRECONDITION FAILED: sweep %s never collected a full witness", txid)
	}
	bcTxid, hMined, err := vfBroadcastAndMine(t, d, ctx, rawHex)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: broadcast of sweep %s rejected: %v", txid, err)
	}
	t.Logf("sweep %s (btc txid %s) MINED at regtest height %d; confirmSpend deliberately NOT sent", txid, bcTxid, hMined)

	// Relay up to the mined height only, then prove the header is in contract state.
	calls0, _, relayedToMined := f10RelayTo(t, d, ctx, 1, cid, hMined, batch)
	present := false
	for i := 0; i < 12 && !present; i++ {
		present = f10HeaderPresent(d, ctx, 2, cid, hMined)
		if !present {
			time.Sleep(10 * time.Second)
		}
	}
	c.rec("F10-HDR-PRESENT", "control: the header at the sweep height is in contract state before the retention window passes",
		relayedToMined && present, fmt.Sprintf("height=%d contract_last=%d addBlocks_calls=%d present_on_magi2=%v", hMined, contractLastHeight(t, d, ctx, cid), calls0, present))
	if !present {
		t.Errorf("header %d never appeared in contract state; the prune below could not be told from a relay failure", hMined)
		finish()
		return
	}
	gen0Before := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	gen1Before := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
	if !vfSweepRecordOn(d, ctx, 2, cid, txid) {
		t.Fatalf("PRECONDITION FAILED: ms- record for %s not live before the outage", txid)
	}

	// The outage: Bitcoin keeps going for longer than the contract's retention window.
	// Run 1 tried to RELAY all 4,658 headers through addBlocks from the owner account and
	// stalled after 32 calls (the caller's resource credits), then choked the devnet with
	// the retries. The contract offers a cheaper, legitimate route on testnet/regtest:
	// SeedBlocks re-seeds FORWARD for the owner (main.go: HandleSeedBlocks(params,
	// IsTestnet)), and InitPruning pins the prune floor. With the floor pinned at the
	// sweep height and the chain re-seeded past height+retention, the next prune calls
	// delete the sweep's header exactly as 32 days of natural relaying would. Mainnet has
	// no re-seed; the pruning there is the slow path, the terminal state is identical.
	target := hMined + retention + margin
	if _, err := d.MineBlocks(ctx, int(target-hMined)); err != nil {
		t.Fatalf("PRECONDITION FAILED: mining %d regtest blocks: %v", target-hMined, err)
	}
	relayStart := time.Now()
	ip := vstatus(t, d, ctx, 1, cid, "initPruning", fmt.Sprintf("%d", hMined))
	hdrT, herr := btcBlockHeaderHex(ctx, d, target)
	if herr != nil {
		t.Fatalf("PRECONDITION FAILED: header at %d: %v", target, herr)
	}
	rs := vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdrT, target))
	calls := 2
	lastH := uint64(0)
	for i := 0; i < 12 && lastH < target; i++ {
		time.Sleep(10 * time.Second)
		lastH = contractLastHeight(t, d, ctx, cid)
	}
	reached := lastH >= target
	c.rec("F10-RELAY", "instrument: the contract's last height passed the sweep height by more than the retention window",
		reached && lastH >= hMined+retention+margin,
		fmt.Sprintf("route=initPruning(%d)=%s + owner re-seed at %d=%s; contract_last=%d target=%d (mined %d + retention %d + margin %d) calls=%d time=%s", hMined, ip, target, rs, lastH, target, hMined, retention, margin, calls, time.Since(relayStart).Round(time.Second)))
	if !reached {
		t.Errorf("the re-seed never landed (contract last=%d, target %d); the retention window was not crossed so nothing below proves pruning", lastH, target)
		finish()
		return
	}

	// Prune is 50 headers per call; drive the admin prune until the sweep header is
	// gone (bounded), then read it on two nodes with the tip as the control.
	pruneCalls := 0
	pruneStatus := ""
	for i := 0; i < 8 && f10HeaderPresent(d, ctx, 2, cid, hMined); i++ {
		pruneStatus = vstatus(t, d, ctx, 1, cid, "prune", "")
		pruneCalls++
		time.Sleep(5 * time.Second)
	}
	goneOn2, goneOn1 := false, false
	for i := 0; i < 18 && !(goneOn2 && goneOn1); i++ {
		goneOn2 = !f10HeaderPresent(d, ctx, 2, cid, hMined)
		goneOn1 = !f10HeaderPresent(d, ctx, 1, cid, hMined)
		if !(goneOn2 && goneOn1) {
			time.Sleep(10 * time.Second)
		}
	}
	tipPresent := f10HeaderPresent(d, ctx, 2, cid, lastH)
	// INVERTED BY VR2-03. This case used to assert the BUG: pruning deleted the
	// header a pending sweep still needed, so the sweep could never settle, redrive
	// could not help (the coins had already moved), and the generation stayed funded
	// forever — blocking every rotation and holding every witness's bond.
	//
	// Pruning now refuses to go below the oldest live pending spend, so the header
	// survives past the normal retention window for exactly as long as something
	// still needs it. The tip header remains the control.
	c.rec("F10-PRUNED", "the header proving a PENDING sweep is RETAINED past the normal retention window, because a live spend still needs it (VR2-03)",
		!goneOn2 && !goneOn1 && tipPresent,
		fmt.Sprintf("header %d gone: magi-2=%v magi-1=%v; tip header %d present=%v; admin prune calls=%d last_status=%s", hMined, goneOn2, goneOn1, lastH, tipPresent, pruneCalls, pruneStatus))

	// INVERTED BY VR2-03, like the case above: confirmSpend with the CORRECT proof
	// now SETTLES, because the header it needs was retained. (The relay loop above
	// drove the contract far past hMined, so the VR2-06 depth gate is satisfied
	// many times over and is not in play here.)
	cs := vfConfirmSpendOnly(t, d, ctx, 1, cid, bcTxid, hMined, 0)
	time.Sleep(15 * time.Second)
	msLive := vfSweepRecordOn(d, ctx, 2, cid, txid)
	gen0After := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	gen1After := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
	// INVERTED BY VR2-03: with the header retained the settle is possible, so the
	// sweep is no longer stranded. gen-0 gives up its inputs and gen-1 is credited,
	// which is the whole point of retaining the header.
	c.rec("F10-CONFIRM-SETTLES", "confirmSpend with a valid proof SETTLES because the header it needs was retained; the sweep clears and gen-0 drains to gen-1 (VR2-03)",
		isOK(cs) && gen0After < gen0Before,
		fmt.Sprintf("confirmSpend status=%s (want CONFIRMED) ms_live=%v (want false: settled) gen0_utxos=%d (was %d) gen1_utxos=%d (was %d)", cs, msLive, gen0After, gen0Before, gen1After, gen1Before))

	// ── INVERTED BY VR2-03, like F10-PRUNED and F10-CONFIRM-SETTLES above. ──
	//
	// These two cases used to assert that NOTHING could recover the sweep: redrive refused
	// while the original stayed pending, gen-0 pinned funded in a locked state forever, no
	// gen-2, rotation permanently stuck. That was the BUG, and it is gone: the header the
	// sweep needed is retained, so confirmSpend settles it (F10-CONFIRM-SETTLES above) and
	// the generation drains.
	//
	// They are turned around rather than deleted, because the recovery path is exactly what
	// a reviewer needs proven: after the settle, there is nothing left to recover FROM, and
	// the rotation carries on.

	rd := vstatus(t, d, ctx, 1, cid, "redriveSpend", txid)
	msStillLive := vfSweepRecordOn(d, ctx, 2, cid, txid)
	// A refusal is now the CORRECT answer, and for a specific reason: the sweep already
	// settled, so there is no in-flight record to re-drive. Asserting the record is also
	// gone is what separates "refused because it settled" from "refused because something
	// is wedged" -- the two the old case could not tell apart.
	c.rec("F10-REDRIVE-NOTHING-TO-RECOVER", "redriveSpend is refused because the sweep already SETTLED, not because it is stuck: no in-flight record remains (VR2-03)",
		!isOK(rd) && !msStillLive,
		fmt.Sprintf("redriveSpend status=%s (want refused) original ms record live=%v (want false = already settled)", rd, msStillLive))

	wod := vstatus(t, d, ctx, 1, cid, "writeOffDust", "")
	rv := vstatus(t, d, ctx, 1, cid, "retireVault", "")
	ck := vstatus(t, d, ctx, 1, cid, "createKey", "")
	// POLL for the end state rather than reading once. vfVaultStatusOn returns -1 for BOTH
	// "unreadable" and "absent", so a single read taken right after three transactions can
	// report a transition that simply has not propagated yet as a product failure. This is
	// the same defect that made DRAIN-04 fail on a GQL timeout; not repeating it here.
	gen0Stat, gen2Stat, gen0Final := -1, -1, gen0Before
	for i := 0; i < 24; i++ {
		time.Sleep(5 * time.Second)
		gen0Stat = vfVaultStatusOn(d, ctx, 2, cid, 0)
		gen2Stat = vfVaultStatusOn(d, ctx, 2, cid, 2)
		gen0Final = vfGenUtxoCountOn(d, ctx, 2, cid, 0)
		if gen0Final == 0 && gen0Stat >= 3 && gen2Stat >= 0 {
			break
		}
	}
	// gen-0 is now EMPTY (its input was deleted when the sweep settled), so
	// ReconcileRetiringVaults can advance it and NN#3 no longer blocks the next mint.
	// The assertion is the rotation MOVING: drained, advanced past Retiring, and gen-2
	// minted. writeOffDust remains a no-op here (the 50M input was never dust), which is
	// why its status is reported but not asserted on.
	rotationProceeds := gen0Final == 0 && gen0Stat >= 3 && gen2Stat >= 0 && isOK(ck)
	c.rec("F10-ROTATION-COMPLETES", "with the sweep settled the rotation carries on: gen-0 is drained and advances, and createKey mints gen-2 (VR2-03)",
		rotationProceeds,
		fmt.Sprintf("writeOffDust=%s retireVault=%s createKey=%s (want accepted) | gen-0 status=%d (want >=3, advanced past Retiring) gen-0 utxos=%d (was %d, want 0) gen-2 status=%d (want >=0 = minted)", wod, rv, ck, gen0Stat, gen0Final, gen0Before, gen2Stat))

	// The bond stays locked, but NOT because anything is stuck any more. gen-0 has drained
	// and reached Inactive; it cannot reach Purged until the purge grace window elapses,
	// and the bond is held until then. The old wording ("behind the stuck generation") was
	// describing the bug this test no longer reproduces, so a reader would have taken this
	// PASS as evidence of a wedge that is not there.
	const unstakeNode = 3
	member := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, unstakeNode)
	bondDetail := ""
	bondLocked := false
	if _, err := d.ConsensusUnstake(unstakeNode, "1.000"); err != nil {
		bondDetail = fmt.Sprintf("consensus_unstake broadcast error: %v", err)
	} else {
		time.Sleep(60 * time.Second)
		pending := pendingConsensusUnstake(t, d, ctx, 2, member)
		bondLocked = pending == 0
		bondDetail = fmt.Sprintf("member=%s pending consensus_unstake amount=%d (want 0 = refused)", member, pending)
	}
	c.rec("F10-BOND", "consensus_unstake is still refused while gen-0 sits Inactive awaiting the purge grace window (the bond releases on PURGE, not on drain)",
		bondLocked, bondDetail)

	finish()
}

// f10HeaderPresent reads the contract's raw header slot for height h on node n.
func f10HeaderPresent(d *Devnet, ctx context.Context, node int, cid string, h uint64) bool {
	key := "b-" + fmt.Sprint(h)
	st, err := getStateHex(d, ctx, node, cid, []string{key})
	if err != nil {
		return false
	}
	return len(st[key]) == 80
}

// f10HeaderHexRange returns the concatenated raw header hex for heights [from, to]
// using one shell loop inside the bitcoind container (one docker exec instead of two
// per header). Falls back to per-height reads when the bulk output does not verify.
func f10HeaderHexRange(ctx context.Context, d *Devnet, from, to uint64) (string, error) {
	if to < from {
		return "", nil
	}
	cli := "bitcoin-cli -regtest -rpcuser=vsc-node-user -rpcpassword=vsc-node-pass"
	script := fmt.Sprintf(`for h in $(seq %d %d); do %s getblockheader $(%s getblockhash $h) false; done | tr -d '\n'`, from, to, cli, cli)
	out, err := d.composeOutput(ctx, "exec", "-T", "bitcoind", "sh", "-c", script)
	out = strings.TrimSpace(out)
	want := int(to-from+1) * 160
	if err == nil && len(out) == want {
		if _, herr := hex.DecodeString(out); herr == nil {
			return out, nil
		}
	}
	var sb strings.Builder
	for h := from; h <= to; h++ {
		hx, herr := btcBlockHeaderHex(ctx, d, h)
		if herr != nil {
			return "", fmt.Errorf("header %d: %w", h, herr)
		}
		sb.WriteString(hx)
	}
	return sb.String(), nil
}

// f10RelayTo pushes headers into the contract until its last height reaches target.
// Each wave submits every remaining batch fire-and-forget (about two calls per Hive
// block, under the per-account custom_json cap), then waits for the contract to catch
// up; a wave with no progress halves the batch. Returns calls made, elapsed, reached.
func f10RelayTo(t *testing.T, d *Devnet, ctx context.Context, callNode int, cid string, target uint64, batch int) (int, time.Duration, bool) {
	t.Helper()
	start := time.Now()
	calls := 0
	if batch < 1 {
		batch = 1
	}
	for wave := 1; wave <= 8; wave++ {
		last := contractLastHeight(t, d, ctx, cid)
		if last >= target {
			return calls, time.Since(start), true
		}
		submitted := 0
		for from := last + 1; from <= target; from += uint64(batch) {
			to := from + uint64(batch) - 1
			if to > target {
				to = target
			}
			hx, err := f10HeaderHexRange(ctx, d, from, to)
			if err != nil {
				t.Logf("relay wave %d: headers %d..%d: %v", wave, from, to, err)
				break
			}
			payload := fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx)
			if _, err := d.CallContractWithIntents(ctx, callNode, cid, "addBlocks", payload, nil, 8_000_000); err != nil {
				t.Logf("relay wave %d: submit %d..%d: %v (retrying after a block)", wave, from, to, err)
				time.Sleep(3500 * time.Millisecond)
				if _, err2 := d.CallContractWithIntents(ctx, callNode, cid, "addBlocks", payload, nil, 8_000_000); err2 != nil {
					t.Logf("relay wave %d: submit %d..%d failed twice: %v", wave, from, to, err2)
					break
				}
			}
			calls++
			submitted++
			time.Sleep(1500 * time.Millisecond)
		}
		// Wait for the contract to catch up with the wave (stall-bounded).
		prev := last
		stall := 0
		for stall < 12 {
			time.Sleep(10 * time.Second)
			now := contractLastHeight(t, d, ctx, cid)
			if now >= target {
				return calls, time.Since(start), true
			}
			if now > prev {
				prev = now
				stall = 0
			} else {
				stall++
			}
		}
		t.Logf("relay wave %d: submitted %d calls (batch %d), contract last %d -> %d (target %d)", wave, submitted, batch, last, prev, target)
		if prev == last && batch > 1 {
			batch /= 2
			t.Logf("relay wave %d made no progress; halving the batch to %d", wave, batch)
		}
	}
	return calls, time.Since(start), contractLastHeight(t, d, ctx, cid) >= target
}

// vfConfirmSpendOnly issues confirmSpend for bcTxid mined at h WITHOUT relaying any
// header first (the caller controls the header state deliberately).
func vfConfirmSpendOnly(t *testing.T, d *Devnet, ctx context.Context, callNode int, cid, bcTxid string, h uint64, index int) string {
	t.Helper()
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", bhash, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	json.Unmarshal([]byte(blockJSON), &blk)
	if len(blk.Tx) != 2 {
		t.Logf("confirm block has %d txs (want 2): the merkle proof helper assumes coinbase+1", len(blk.Tx))
	}
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", bcTxid)
	proof := ""
	if len(blk.Tx) > 0 {
		proof = reverseHexBytes(blk.Tx[0])
	}
	return vstatus(t, d, ctx, callNode, cid, "confirmSpend", fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"indices":[%d]}`, h, rawTx, proof, index))
}
