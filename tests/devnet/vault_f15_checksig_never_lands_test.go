package devnet

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultF15CheckSigNeverLands is the F15 failure-state test of the BTC
// vault-rotation-v2 suite. It proves BRK-2 live: a generation whose committee
// agreed on a key but cannot produce the check-signature is never activated, the
// escape hatch works, and a re-mint gets a fresh generation number.
//
// WHAT IT REPRODUCES
// A rotation keygen completes (all 5 parties agree on the gen-1 pubkey and the
// keygen commitment lands on chain), and only THEN does the operator lose 2 of the
// 5 nodes. Agreement on a pubkey is done, but the TSS signing threshold
// (ceil(2n/3)-1 = 3, so 4 live parties) is now unreachable, so the canonical BRK-2
// check-signature that the state engine enqueued the instant the key flipped
// active (state_engine.go maybeEnqueueVaultCheckSig) can never be produced. This is
// the "agreed but unsignable key" incident: activating it would route the vault's
// funds into an address only the single CSV backup key could ever spend.
//
// WHAT IT PROVES
//  1. F15-REFUSED: activateKey is REFUSED for as long as the check-signature is
//     missing (contract attestPrimaryKey, BRK-2 clause), and gen-1 stays Pending.
//     Because a quorum halt keeps the transaction status non-terminal, the verdict
//     is read from contract STATE on a live node, not from the tx status.
//  2. F15-DISCARD: the never-brick escape hatch works while the chain is halted:
//     discardPendingKey removes the stalled gen-1 from the registry, executed
//     locally by the 3 surviving nodes, and the live gen-0 vault is untouched.
//  3. F15-REMINT: after the committee is restored, a fresh rotation mints
//     generation 2, NEVER reusing the discarded number 1 (monotonic generations,
//     so a partially completed keygen can never collide with a later one), and
//     gen-0 then drains into gen-2 with a real signed migration sweep.
//  4. F15-IDENT: the vault contract state is byte-identical on all 5 nodes at the
//     end, including the two that replayed the halted window after restarting.
//
// TIMING SUBTLETY (why this test polls instead of using WaitForTssKey directly)
// The check-signature request is enqueued the moment the keygen commitment is
// processed, and it is signed at the next sign interval (SignInterval 10 blocks,
// about 30 seconds). If the two nodes are stopped after that interval elapses the
// signature lands, gen-1 activates, and the F15-REFUSED precondition is gone. So
// the test polls tss_keys itself every 2 seconds and stops magi-4 and magi-5 in
// PARALLEL the instant the status flips to active. If the signature still won it,
// the run is recorded honestly as PRECONDITION-MISSED (a FAIL that says "rerun"),
// never as a pass.
//
// RUN
//
//	VAULT_F15_RUN=1 go test -v -run TestVaultF15CheckSigNeverLands -timeout 50m ./tests/devnet/
//
// Set DEVNET_KEEP=1 to leave the devnet running for post-mortem inspection, and
// BTC_MAPPING_WASM_PATH to point at a different contract build.
func TestVaultF15CheckSigNeverLands(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F15_RUN") == "" {
		t.Skip("set VAULT_F15_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// hpin AFTER genesis (about block 190) so gen-0 is minted on the v2-off genesis
	// path (no fresh-genesis deadlock) and v2 is live for the rotation.
	const hpin uint64 = 400

	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 45*time.Minute)

	c := &vfCase{t: t}

	// Setup: deploy, seed headers, wire the oracle, mint + register gen-0, fund it.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F15 check-signature never lands")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	keyId1 := cid + "-mainv1"

	// ---- step 1: start the gen-1 keygen and let it COMPLETE ----
	if s := vstatus(t, d, ctx, 1, cid, "createKey", ""); !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: createKey (gen-1) status=%s, gen-1 was never minted so there is no pending generation to refuse, discard or re-mint", s)
	}
	t.Logf("gen-1 createKey accepted, waiting for the keygen commitment with a tight 2s poll so magi-4 and magi-5 can be stopped the instant the key flips active")

	kd1, lastSeen, err := f15WaitKeyActive(ctx, d, 1, keyId1, 10*time.Minute)
	if err != nil {
		all, _ := d.GetTssKeys(ctx, 1, bson.M{})
		for _, k := range all {
			t.Logf("  tss_key id=%s status=%s epoch=%d", k.Id, k.Status, k.Epoch)
		}
		t.Fatalf("PRECONDITION FAILED: gen-1 keygen never reached status active on magi-1 (last status seen %q): %v. The committee never agreed on a key, so the agreed-but-unsignable scenario was never established", lastSeen, err)
	}
	primary1 := kd1.PublicKey
	t.Logf("gen-1 keygen active: keyId=%s epoch=%d pubkey=%s", keyId1, kd1.Epoch, primary1)

	// ---- step 1b: drop 2 of 5 immediately, in parallel, to beat the sign interval ----
	stopTook := f15StopFast(t, d, ctx, []int{4, 5})
	t.Logf("magi-4 and magi-5 stopped %v after the key flipped active (SignInterval is 10 hive blocks, about 30s, so anything well under that wins the race)", stopTook)

	// The exact BRK-2 check message the state engine enqueued for this key. Having
	// it makes the precondition instrument DIRECT (is the signature there or not)
	// instead of inferred from an activateKey outcome.
	var checkMsg []byte
	if pub, derr := hex.DecodeString(primary1); derr == nil && len(pub) == btcvault.CompressedPubKeyLen {
		checkMsg = btcvault.CheckSigMessage(keyId1, 1, pub)
		t.Logf("BRK-2 check message for %s gen-1: %s", keyId1, hex.EncodeToString(checkMsg))
	} else {
		t.Logf("could not recompute the BRK-2 check message (pubkey %q decode err=%v), falling back to the activateKey outcome as the precondition instrument", primary1, derr)
	}

	// ---- step 1c: observe the halt, which doubles as the settle window for any
	// check-signature that was already in flight when the nodes went down ----
	grew, haltStart, haltLast := vfGrewWithin(d, ctx, 1, 60*time.Second)
	t.Logf("halt instrument: block_headers max slot on magi-1 start=%d last=%d over 60s, grew=%v (3 of 5 cannot reach the 4 of 5 BLS quorum)", haltStart, haltLast, grew)

	if checkMsg != nil && vfSignatureLanded(d, ctx, 1, keyId1, checkMsg) {
		t.Logf("WARNING: the BRK-2 check-signature for gen-1 ALREADY landed on magi-1 before the two nodes were stopped")
	}

	// ---- step 1d: register the pending generation's keys DURING the halt ----
	// For a rotation (non-genesis) RegisterVaultKeys only stores the keys, it does
	// not attest, so this succeeds locally even with the check-signature missing.
	// Confirming the stored primary matters: without it an activateKey refusal
	// could be blamed on a missing registration instead of on BRK-2.
	regStatus := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary1, backupPubKeyG))
	registered := false
	regDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(regDeadline) {
		if f15PrimaryHexOn(d, ctx, 1, cid, 1) == primary1 {
			registered = true
			break
		}
		time.Sleep(10 * time.Second)
	}
	t.Logf("registerPublicKey during the halt: status=%s, gen-1 primary stored on magi-1=%v (stored=%q)",
		regStatus, registered, f15PrimaryHexOn(d, ctx, 1, cid, 1))

	// ---- step 2: F15-REFUSED, activateKey must be refused while the check-sig is absent ----
	anyOK := false
	statuses := ""
	for i := 0; i < 3; i++ {
		s := vstatus(t, d, ctx, 1, cid, "activateKey", "")
		statuses += fmt.Sprintf(" try%d=%q", i+1, s)
		if isOK(s) {
			anyOK = true
			break
		}
		if i < 2 {
			time.Sleep(20 * time.Second)
		}
	}
	gen1Status := vfVaultStatusOn(d, ctx, 1, cid, 1)
	gen0Status := vfVaultStatusOn(d, ctx, 1, cid, 0)
	sigLanded := checkMsg != nil && vfSignatureLanded(d, ctx, 1, keyId1, checkMsg)

	// PRECONDITION-MISSED means the check-signature won the race, so gen-1 was
	// legitimately activatable and F15 proved nothing. Report it as such, never as
	// a pass. The direct instrument is the landed signature; a successful
	// activateKey is the fallback instrument when the message could not be
	// recomputed.
	missed := sigLanded || anyOK
	if missed {
		c.rec("F15-REFUSED", "activateKey is refused while the BRK-2 check-signature is missing (gen-1 stays Pending)",
			false, fmt.Sprintf("PRECONDITION-MISSED, RERUN: the check-signature landed before magi-4 and magi-5 were stopped (stop took %v, sigLanded=%v, activateKey ok=%v,%s). The agreed-but-unsignable state was never established, so nothing below could be tested. Rerun the test.",
				stopTook, sigLanded, anyOK, statuses))
	} else {
		c.rec("F15-REFUSED", "activateKey is refused while the BRK-2 check-signature is missing (gen-1 stays Pending)",
			gen1Status == int(btcvault.VaultStatusPending),
			fmt.Sprintf("3 activateKey tries 20s apart, none ok:%s; check-signature landed on magi-1=%v; gen-1 status on magi-1=%d (want 0 Pending); gen-0 status=%d (1 Active, untouched); registered=%v",
				statuses, sigLanded, gen1Status, gen0Status, registered))
	}

	// Steps 3 and 4 run in a closure so an unrecoverable failure can stop the
	// sequence while still letting the restart, F15-IDENT and the summary run.
	func() {
		if missed {
			c.rec("F15-DISCARD", "discardPendingKey removes the stalled gen-1 during the halt", false,
				"NOT EXERCISED: the F15-REFUSED precondition was missed, there was no stalled pending generation to discard")
			c.rec("F15-REMINT", "a re-mint after the discard gets a FRESH generation number (never reuses 1)", false,
				"NOT EXERCISED: the F15-REFUSED precondition was missed, nothing was discarded so there is no re-mint to check")
			return
		}

		// ---- step 3: F15-DISCARD, the never-brick escape hatch during the halt ----
		discardStatus := vstatus(t, d, ctx, 1, cid, "discardPendingKey", "")
		discarded := false
		discardDeadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(discardDeadline) {
			if vfVaultStatusOn(d, ctx, 1, cid, 1) == -1 {
				discarded = true
				break
			}
			time.Sleep(10 * time.Second)
		}
		c.rec("F15-DISCARD", "discardPendingKey removes the stalled gen-1 during the halt (executed locally by the surviving nodes)",
			discarded, fmt.Sprintf("tx status=%s (non-terminal is expected during a halt, the verdict is contract STATE); gen-1 status on magi-1=%d (want -1 absent); gen-0 status=%d (want 1 Active, the live vault is never touched)",
				discardStatus, vfVaultStatusOn(d, ctx, 1, cid, 1), vfVaultStatusOn(d, ctx, 1, cid, 0)))
		if !discarded {
			t.Errorf("gen-1 was never removed from the registry, the re-mint below would test a different state")
			return
		}

		// ---- step 4a: restore the committee ----
		vfStartNodes(t, d, ctx, []int{4, 5})
		resumed, resStart, resLast := vfGrewWithin(d, ctx, 1, 5*time.Minute)
		t.Logf("resume: block_headers max slot on magi-1 start=%d last=%d over 5m, grew=%v", resStart, resLast, resumed)
		if !resumed {
			t.Errorf("VSC block production did not resume within 5m after restarting magi-4 and magi-5 (slot %d -> %d)", resStart, resLast)
		}

		// ---- step 4b: F15-REMINT, the fresh generation number ----
		// vfRotate waits for the key named mainv2. If the contract had rolled the
		// counter back and reused generation 1 this would time out, so the wait
		// itself is the monotonicity assertion.
		primary2, rotated := vfRotate(t, d, ctx, cid, 2)
		gen1After := vfVaultStatusOn(d, ctx, 2, cid, 1)
		gen2After := vfVaultStatusOn(d, ctx, 2, cid, 2)
		c.rec("F15-REMINT", "the re-mint gets a FRESH generation number (gen-2), the discarded gen-1 is never reused",
			rotated && gen1After == -1 && gen2After == int(btcvault.VaultStatusActive),
			fmt.Sprintf("rotate to gen-2 ok=%v, key=%s-mainv2 pubkey=%s; gen-1 status on magi-2=%d (want -1 absent); gen-2 status=%d (want 1 Active); gen-0 status=%d (want 2 Retiring)",
				rotated, cid, primary2, gen1After, gen2After, vfVaultStatusOn(d, ctx, 2, cid, 0)))
		if !rotated {
			return
		}

		// ---- step 4c: F15-REMINT, gen-0 really drains into the re-minted gen-2 ----
		gen0Before := genUtxoCount(t, d, ctx, cid, 0)
		if gen0Before <= 0 {
			c.rec("F15-REMINT", "gen-0 drains into the re-minted gen-2 with a signed migration sweep", false,
				fmt.Sprintf("PRECONDITION: gen-0 held %d UTXOs before the sweep, there was nothing to migrate", gen0Before))
			return
		}
		fundFeeReserve(t, d, ctx, cid, primary2, backupPubKeyG, 10_000_000)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary2, backupPubKeyG)
		gen0After := genUtxoCount(t, d, ctx, cid, 0)
		c.rec("F15-REMINT", "gen-0 drains into the re-minted gen-2 with a signed migration sweep (the vault survived the stalled rotation)",
			gen0After >= 0 && gen0After < gen0Before,
			fmt.Sprintf("gen-0 UTXO count %d -> %d, gen-2 UTXO count=%d", gen0Before, gen0After, genUtxoCount(t, d, ctx, cid, 2)))
	}()

	// Make sure the two nodes are back for the identity check even if the closure
	// returned before restarting them.
	f15EnsureUp(t, d, ctx, []int{4, 5})

	// ---- step 5: F15-IDENT, no fork survived the halt ----
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F15-IDENT")
	c.summary("F15")
	t.Logf("F15 COMPLETE CONTRACT=%s", cid)
}

// f15WaitKeyActive polls ONE node's tss_keys every 2 seconds until the key reaches
// status active, and returns the doc plus the last status seen. The tight interval
// is the point: the BRK-2 check-signature request is enqueued the moment the keygen
// commitment is processed and is signed at the next sign interval (10 hive blocks,
// about 30 seconds), so the caller needs the earliest possible notice to stop nodes
// before that. WaitForTssKey polls every 5 seconds, which can burn most of the
// window on its own.
func f15WaitKeyActive(ctx context.Context, d *Devnet, node int, keyId string, timeout time.Duration) (*TssKeyDoc, string, error) {
	deadline := time.Now().Add(timeout)
	last := "absent"
	for {
		docs, err := d.GetTssKeys(ctx, node, bson.M{"id": keyId})
		if err == nil && len(docs) > 0 {
			last = docs[0].Status
			if docs[0].Status == "active" {
				doc := docs[0]
				return &doc, last, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, last, fmt.Errorf("timeout after %v waiting for tss_key %s to reach active on magi-%d", timeout, keyId, node)
		}
		select {
		case <-ctx.Done():
			return nil, last, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// f15StopFast stops several magi nodes CONCURRENTLY and reports how long it took.
// vfStopNodes stops them one after another, and each docker compose stop can take
// seconds; in F15 that latency is subtracted straight from the window in which the
// check-signature must be prevented, so the two stops overlap here instead.
func f15StopFast(t *testing.T, d *Devnet, ctx context.Context, nodes []int) time.Duration {
	t.Helper()
	start := time.Now()
	errs := make([]error, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(idx, node int) {
			defer wg.Done()
			errs[idx] = d.StopNode(ctx, node)
		}(i, n)
	}
	wg.Wait()
	elapsed := time.Since(start)
	for i, n := range nodes {
		if errs[i] != nil {
			t.Fatalf("stopping magi-%d: %v", n, errs[i])
		}
		t.Logf("stopped magi-%d", n)
	}
	return elapsed
}

// f15EnsureUp starts nodes that may already be running. A start on a running
// container is a no-op, so a failure here is logged, not fatal: the identity check
// that follows reports an unreadable node on its own.
func f15EnsureUp(t *testing.T, d *Devnet, ctx context.Context, nodes []int) {
	t.Helper()
	for _, n := range nodes {
		if err := d.StartNode(ctx, n); err != nil {
			t.Logf("could not start magi-%d (it may already be running): %v", n, err)
		}
	}
}

// f15PrimaryHexOn returns the primary pubkey hex stored in the contract's vault
// registry for a generation, as read from one node ("" if the generation is absent
// or the node cannot be read).
func f15PrimaryHexOn(d *Devnet, ctx context.Context, node int, cid string, gen uint32) string {
	for _, v := range vfVaultRegistryOn(d, ctx, node, cid) {
		if v.Generation == gen {
			return hex.EncodeToString(v.Primary)
		}
	}
	return ""
}
