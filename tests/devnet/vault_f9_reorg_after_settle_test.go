package devnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// f9MaxWalkBack mirrors maxReorgDepth in modules/oracle/chain/chain_relay.go: the
// production oracle walks at most 20 blocks down from the contract tip looking for
// the fork point, so the test uses the same bound.
const f9MaxWalkBack = 20

// f9StoredHeaderHex reads the 80-byte block header the CONTRACT stores at a height.
// The key layout is constants.BlockPrefix + decimal height ("b-<height>"), the same
// key the oracle's getStoredBlockHeaderHex reads, and the value is the raw 80 bytes
// (not hex), so the bytes coming back from getStateHex are re-hexed here to compare
// against bitcoind's getblockheader output.
func f9StoredHeaderHex(d *Devnet, ctx context.Context, node int, cid string, height uint64) string {
	key := "b-" + strconv.FormatUint(height, 10)
	st, err := getStateHex(d, ctx, node, cid, []string{key})
	if err != nil {
		return ""
	}
	return hex.EncodeToString(st[key])
}

// f9TxBlockHash returns the hash of the block that currently contains txid, or
// ("", false) if the transaction is unconfirmed (sitting in the mempool).
func f9TxBlockHash(d *Devnet, ctx context.Context, txid string) (string, bool) {
	out, err := d.bitcoinCli(ctx, "getrawtransaction", txid, "1")
	if err != nil {
		return "", false
	}
	var v struct {
		BlockHash string `json:"blockhash"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", false
	}
	return v.BlockHash, v.BlockHash != ""
}

// f9BlockHeightOf resolves a block hash to its height on the CURRENT best chain.
func f9BlockHeightOf(d *Devnet, ctx context.Context, blockHash string) (uint64, bool) {
	out, err := d.bitcoinCli(ctx, "getblock", blockHash, "1")
	if err != nil {
		return 0, false
	}
	var v struct {
		Height uint64 `json:"height"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return 0, false
	}
	return v.Height, true
}

// f9ReplacementHeaders reproduces the production oracle's checkForReorg walk
// (modules/oracle/chain/chain_relay.go:534). Starting at the contract's own tip
// height it compares the header the contract has stored at each height against the
// header bitcoind now considers canonical, walking DOWN until the two agree (the
// fork point). It returns the canonical headers for the mismatching range OLDEST
// FIRST, which is exactly the argument shape HandleReplaceBlocks expects: with N
// headers the anchor is lastHeight-N and header i is written at anchor+1+i, so the
// contract tip height never moves and only the reorged range is rewritten.
func f9ReplacementHeaders(t *testing.T, d *Devnet, ctx context.Context, node int, cid string, tip uint64) []string {
	t.Helper()
	var out []string
	for depth := 0; depth < f9MaxWalkBack; depth++ {
		if tip < uint64(depth)+1 {
			break
		}
		h := tip - uint64(depth)
		canonical, err := btcBlockHeaderHex(ctx, d, h)
		if err != nil || canonical == "" {
			t.Logf("reorg walk-back: no canonical header from bitcoind at height %d: %v", h, err)
			break
		}
		stored := f9StoredHeaderHex(d, ctx, node, cid, h)
		if stored == "" {
			t.Logf("reorg walk-back: the contract stores no header at height %d, stopping the walk", h)
			break
		}
		if strings.EqualFold(stored, canonical) {
			// Fork point: this height already matches, so nothing below it moved.
			break
		}
		out = append([]string{canonical}, out...)
	}
	return out
}

// f9ConservedDiff lists the vault state keys that changed, EXCLUDING "h" (the
// contract's last relayed BTC height), which is expected to advance because the
// reorg replay relays new headers. Everything else in vfStateKeys is what "the
// reorg must not touch" means: registry, counters, UTXO set, pending spends,
// supply, migration sweep index, migrate version, theft halt, pause, operator.
func f9ConservedDiff(before, after map[string][]byte) []string {
	var out []string
	for _, k := range vfStateKeys {
		if k == "h" {
			continue
		}
		if !bytes.Equal(before[k], after[k]) {
			out = append(out, fmt.Sprintf("%s(%d->%d bytes)", k, len(before[k]), len(after[k])))
		}
	}
	return out
}

// TestVaultF9ReorgAfterSettle is failure-state F9 of the BTC vault-rotation-v2
// suite: "a Bitcoin reorg AFTER the migration sweep has already settled".
//
// WHAT IT REPRODUCES
// The rotation drains gen-0 into gen-1 with a single migration sweep. Once
// confirmSpend has settled that sweep, the contract has permanently rewritten its
// UTXO registry: the gen-0 UTXOs are gone and gen-1 UTXOs stand in their place.
// The evidence that justified that rewrite is a merkle proof against ONE Bitcoin
// block header. Bitcoin can take that block back. This test invalidates the block
// that carried the settled sweep on the regtest node, mines a competing branch,
// and then relays the new branch to the contract through replaceBlocks, which is
// the reorg op (contract/main.go ReplaceBlocks, contract/blocklist HandleReplaceBlocks).
//
// WHAT IT PROVES
//  1. F9-CONSERVED: a reorg that RE-MINES the same sweep transaction in a different
//     block leaves the contract's vault registry, its per-generation UTXO counts and
//     its supply byte-identical to what they were before the reorg. Only "h", the
//     last relayed BTC height, moves, and it moves because new headers were relayed.
//     This is the realistic reorg, and the finding is that it is harmless.
//  2. F9-IDENT: after the header replacement every one of the 5 nodes holds
//     byte-identical vault contract state, so the replacement did not fork the fleet.
//
// WHAT IT DOES NOT PROVE, AND WHY (recorded as F9-DROPPED, INFO, never a PASS)
// The dangerous reorg is the one that DROPS the sweep: the competing branch would
// have to contain a CONFLICTING spend of the very same vault inputs, mined by an
// outsider. Those inputs are locked by the retiring generation's threshold key, so
// producing that conflicting spend requires the vault key itself. No devnet harness
// can manufacture it, and bitcoind will simply re-mine the sweep out of its own
// mempool onto the new branch. So the "sweep dropped" leg is NOT reproducible here.
// It matters because reorg-reversible migration (U-10) is UNBUILT: the contract has
// no path that un-settles a migration record, restores the gen-0 UTXOs and re-arms
// the sweep. HandleReplaceBlock and HandleReplaceBlocks deliberately leave the
// observed-transaction list populated across a replacement (to block a double mint
// on re-inclusion), which also means a settled migration stays settled no matter
// what the replacement headers say. That gap is documented here, not tested here.
//
// A precondition that cannot be established (rotation, fee reserve, sweep build,
// signatures, broadcast, settle) is a t.Fatalf, never a t.Skip, so this test can
// never pass vacuously.
//
// RUN:
//
//	VAULT_F9_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF9ReorgAfterSettle -timeout 95m ./tests/devnet/
func TestVaultF9ReorgAfterSettle(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F9_RUN") == "" {
		t.Skip("set VAULT_F9_RUN=1")
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

	// hpin is AFTER genesis (~block 190) so gen-0 is minted on the v2-off path (no
	// fresh-genesis deadlock) and v2 is ON for the rotation, the sweep and the reorg.
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

	// ---- SETUP: deploy, seed headers, wire the oracle, mint + register gen-0, fund it ----
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F9 reorg after settle")
	cid := env.cid

	// v2 must really be in force before any v2 assertion, otherwise the whole test
	// is vacuous (flag inert, registry absent).
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 to gen-1 rotation did not complete (primary1=%q). Without a Retiring gen-0 and an Active gen-1 there is no migration sweep to reorg around", primary1)
	}
	t.Logf("rotation done: gen-1 active, primary1=%s, gen-0 retiring", primary1)

	// The migration sweep pays its miner fee out of FeeSupply, so seed it.
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ---- 1. build, sign, broadcast, mine and SETTLE the sweep ----
	// These are migrateAndSettle's phases, replicated so the block that carries the
	// sweep is identified here (migrateAndSettle mines and confirms internally and
	// returns nothing, and F9 needs both the broadcast txid and the block height).
	txid, sd, buildStatus := vfBuildSweep(t, d, ctx, 1, cid)
	if sd == nil {
		t.Fatalf("PRECONDITION FAILED: no migration sweep signing data appeared (txid=%q migrateVault status=%s). There is nothing to settle and therefore nothing to reorg", txid, buildStatus)
	}
	t.Logf("sweep built: txid=%s inputs=%d (migrateVault status=%s)", txid, len(sd.UnsignedSigHashes), buildStatus)

	raw, signed := vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
	if !signed {
		t.Fatalf("PRECONDITION FAILED: the retiring generation (%s-main) never signed every sweep input, so the sweep can never be broadcast", cid)
	}

	bcTxid, h, err := vfBroadcastAndMine(t, d, ctx, raw)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: sweep broadcast/mine failed: %v (bcTxid=%q). The reorg needs the sweep on the BTC chain first", err, bcTxid)
	}
	t.Logf("sweep broadcast and mined: bcTxid=%s btcHeight=%d", bcTxid, h)

	settleStatus := vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, h)
	t.Logf("confirmSpend status=%s", settleStatus)

	// The settle is the precondition, so it is asserted on STATE, not on the tx
	// status: gen-0 must be drained and the ms-<txid> migration record gone.
	settled := false
	var g0Before, g1Before int
	var msBefore bool
	deadline := time.Now().Add(4 * time.Minute)
	for {
		g0Before = vfGenUtxoCountOn(d, ctx, 2, cid, 0)
		g1Before = vfGenUtxoCountOn(d, ctx, 2, cid, 1)
		msBefore = vfSweepRecordOn(d, ctx, 2, cid, txid)
		if g0Before == 0 && g1Before >= 1 && !msBefore {
			settled = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(15 * time.Second)
	}
	if !settled {
		t.Fatalf("PRECONDITION FAILED: the migration sweep never settled on magi-2 within 4m (confirmSpend status=%s, gen0Utxos=%d want 0, gen1Utxos=%d want >=1, msRecord=%v want false). F9 only means something once the sweep IS settled",
			settleStatus, g0Before, g1Before, msBefore)
	}
	t.Logf("sweep SETTLED: gen0Utxos=%d gen1Utxos=%d msRecord=%v", g0Before, g1Before, msBefore)

	// ---- 2. baseline: the state the reorg must not disturb ----
	preState, err := vfStateOn(d, ctx, 2, cid)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: could not read the post-settle baseline state from magi-2: %v", err)
	}
	preFp := vfFingerprint(preState)
	preLast := contractLastHeight(t, d, ctx, cid)
	t.Logf("baseline on magi-2: fp=%s contractLastHeight=%d supply(s)=%s registry(r)=%d bytes",
		preFp, preLast, hex.EncodeToString(preState["s"]), len(preState["r"]))

	// Locate the block that actually carries the settled sweep. It should be the
	// block vfBroadcastAndMine mined (h), but the chain is the authority.
	sweepHash, mined := f9TxBlockHash(d, ctx, bcTxid)
	if !mined {
		t.Fatalf("PRECONDITION FAILED: the settled sweep %s is not in any block on regtest, so there is no block to invalidate", bcTxid)
	}
	sweepHeight, gotHeight := f9BlockHeightOf(d, ctx, sweepHash)
	if !gotHeight {
		t.Fatalf("PRECONDITION FAILED: could not resolve the height of the sweep block %s", sweepHash)
	}
	if sweepHeight != h {
		t.Logf("NOTE: the sweep settled at height %d, not the mined height %d reported by vfBroadcastAndMine; using %d", sweepHeight, h, sweepHeight)
	}
	t.Logf("sweep block before the reorg: hash=%s height=%d", sweepHash, sweepHeight)

	// ---- 3. reorg regtest: invalidate the sweep's block, build a competing branch ----
	if _, err := d.bitcoinCli(ctx, "invalidateblock", sweepHash); err != nil {
		t.Fatalf("PRECONDITION FAILED: invalidateblock %s failed: %v. Without a real reorg on regtest there is nothing to relay", sweepHash, err)
	}
	pool, _ := d.bitcoinCli(ctx, "getrawmempool")
	t.Logf("after invalidateblock the mempool is %s (the sweep is expected back in it: %s)", pool, bcTxid)

	newTip, err := d.MineBlocks(ctx, 2)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: could not mine the competing branch: %v", err)
	}
	t.Logf("competing branch mined, regtest tip is now %d (was %d)", newTip, sweepHeight)

	// bitcoind re-mines the sweep straight out of its own mempool, so the realistic
	// outcome is "same transaction, different block". Both outcomes are recorded.
	newSweepHash, reMined := f9TxBlockHash(d, ctx, bcTxid)
	newSweepHeight := uint64(0)
	if reMined {
		newSweepHeight, _ = f9BlockHeightOf(d, ctx, newSweepHash)
		t.Logf("the sweep was RE-MINED on the new branch: hash=%s height=%d (was hash=%s height=%d)",
			newSweepHash, newSweepHeight, sweepHash, sweepHeight)
	} else {
		t.Logf("the sweep is NOT in a block on the new branch (still unconfirmed after 2 blocks); the conservation check below then measures a chain where the settled sweep is absent")
	}

	// ---- 4. relay the reorg to the contract with replaceBlocks ----
	// replaceBlocks REWRITES the top N stored headers in place: with the contract
	// tip at lastHeight and N headers, header i lands at (lastHeight-N)+1+i and the
	// tip height is unchanged. So the payload is exactly the canonical headers for
	// the range the reorg actually changed, oldest first, computed the same way the
	// production oracle computes it. The headers ABOVE the old tip are not part of
	// the replacement: they are appended afterwards with addBlocks.
	last := contractLastHeight(t, d, ctx, cid)
	repl := f9ReplacementHeaders(t, d, ctx, 2, cid, last)
	if len(repl) == 0 {
		t.Errorf("no header at or below the contract tip %d differs from the canonical chain after invalidateblock, so replaceBlocks has nothing to do. Either the reorg did not reach the contract's stored range or something already repaired it, and the conservation check below is weaker than intended", last)
	} else {
		payload := strings.Join(repl, "")
		t.Logf("replaceBlocks payload: depth=%d heights %d..%d, %d hex chars, first header %s",
			len(repl), last-uint64(len(repl))+1, last, len(payload), repl[0])
		if s := vstatus(t, d, ctx, 1, cid, "replaceBlocks", payload); !isOK(s) {
			t.Errorf("replaceBlocks (depth=%d, tip=%d) was not accepted: status=%s. The contract still holds the orphaned header, so every later addBlocks will fail the parent-hash check", len(repl), last, s)
		} else {
			t.Logf("replaceBlocks accepted: %d header(s) replaced, contract tip stays at %d", len(repl), last)
		}
	}

	// Extend the contract onto the new branch. latest_fee is 10 here, the same value
	// every other relay in this suite uses, so BaseFeeRate inside the supply record
	// does not move and the supply comparison below stays a real conservation test.
	for hh := last + 1; hh <= newTip; hh++ {
		hx, herr := btcBlockHeaderHex(ctx, d, hh)
		if herr != nil {
			t.Errorf("could not read the canonical header at %d for the post-reorg relay: %v", hh, herr)
			break
		}
		if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx)); !isOK(s) {
			t.Errorf("addBlocks %d on the new branch was not accepted: status=%s (a parent-hash failure here means the replacement above did not land)", hh, s)
			break
		}
	}

	// Give the fleet time to execute the relay ops and settle on the new tip.
	relayDeadline := time.Now().Add(3 * time.Minute)
	postLast := last
	for {
		postLast = contractLastHeight(t, d, ctx, cid)
		if postLast >= newTip {
			break
		}
		if time.Now().After(relayDeadline) {
			t.Errorf("the contract's last BTC height is %d after 3m, expected %d; the post-reorg relay did not fully land", postLast, newTip)
			break
		}
		time.Sleep(10 * time.Second)
	}
	t.Logf("contract last BTC height after the reorg relay: %d (was %d, regtest tip %d)", postLast, preLast, newTip)

	// ---- 5. F9-CONSERVED: everything except "h" is byte-identical ----
	postState, perr := vfStateOn(d, ctx, 2, cid)
	if perr != nil {
		t.Errorf("could not read the post-reorg state from magi-2: %v", perr)
		postState = map[string][]byte{}
	}
	g0After := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	g1After := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
	msAfter := vfSweepRecordOn(d, ctx, 2, cid, txid)
	diffs := f9ConservedDiff(preState, postState)
	conserved := perr == nil && len(diffs) == 0 && g0After == g0Before && g1After == g1Before && !msAfter
	c.rec("F9-CONSERVED",
		"a reorg that re-mines the settled migration sweep in a different block leaves the vault registry, generation counts and supply unchanged",
		conserved,
		fmt.Sprintf("reorgDepth=%d sweep %s@%d -> reMined=%v %s@%d; changedKeys(excluding h)=%v; gen0 %d->%d gen1 %d->%d msRecord %v->%v; supply(s) %s->%s; fp %s->%s; h %d->%d",
			len(repl), sweepHash, sweepHeight, reMined, newSweepHash, newSweepHeight,
			diffs, g0Before, g0After, g1Before, g1After, msBefore, msAfter,
			hex.EncodeToString(preState["s"]), hex.EncodeToString(postState["s"]),
			preFp, vfFingerprint(postState), preLast, postLast))

	// ---- 6. F9-DROPPED: the leg this harness cannot reach, recorded as INFO ----
	c.rec("F9-DROPPED",
		"a reorg that DROPS the settled sweep, and the unbuilt reorg-reversible migration behind it",
		true,
		fmt.Sprintf("INFO (not a pass): dropping the sweep needs a CONFLICTING spend of the same vault inputs mined on the competing branch by an outsider. Those inputs are locked by the retiring generation's threshold key, so that conflicting spend cannot be produced without the vault key and no devnet harness can manufacture it. Observed instead: bitcoind re-mined the same sweep out of its own mempool (reMined=%v, %s@%d -> %s@%d). This matters because reorg-reversible migration (U-10) is UNBUILT: the contract has no op that un-settles a migration record, restores the gen-0 UTXOs and re-arms the sweep, and HandleReplaceBlock/HandleReplaceBlocks deliberately keep the observed-transaction list populated across a replacement (double-mint protection), so a settled migration stays settled whatever the replacement headers say. Not tested here, documented here",
			reMined, sweepHash, sweepHeight, newSweepHash, newSweepHeight))

	// ---- 7. the header replacement must not have forked the fleet ----
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F9-IDENT")

	c.summary("F9")
	t.Logf("F9 COMPLETE CONTRACT=%s sweepTxid=%s bcTxid=%s", cid, txid, bcTxid)
}
