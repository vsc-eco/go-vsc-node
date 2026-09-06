package devnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestVaultF28RedriveRBFMempool is the end-to-end proof the July L7 scaffold
// (l7_redrive_devnet_test.go, ten unimplemented helpers) was meant to give: a migration
// sweep built at a LOW fee sits unconfirmed in a real bitcoind-regtest mempool, the
// operator's redriveSpend builds a higher-fee replacement over the same inputs, the
// committee signs it, bitcoind accepts it as a BIP-125 replacement (the original leaves
// the mempool), the replacement confirms, confirmSpend settles the whole spend group
// (both records gone), the retiring generation drains, and the next createKey is
// admitted again (NN#3 unwedged). TestVaultRedriveSweep proves the contract side of
// the redrive; this test proves the Bitcoin side and the settle.
//
// Fee steering: the contract builds a sweep at the fee rate last relayed by addBlocks
// (`latest_fee`), so one header is relayed with latest_fee 1 before migrateVault and
// later headers with latest_fee 10 so the redrive bumps.
//
//	VAULT_F28_RUN=1 go test -v -run TestVaultF28RedriveRBFMempool -timeout 95m ./tests/devnet/
func TestVaultF28RedriveRBFMempool(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F28_RUN") == "" {
		t.Skip("set VAULT_F28_RUN=1")
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
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)

	c := &vfCase{t: t}

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F28 redrive RBF in a real mempool")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	finish := func() {
		vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F28-IDENT")
		c.summary("F28")
		t.Logf("F28 COMPLETE CONTRACT=%s", cid)
	}

	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: rotation did not complete (primary1=%q)", primary1)
	}
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ---- 1. build the sweep at 1 sat/vB ----
	h1, _ := d.MineBlocks(ctx, 1)
	hx1, _ := btcBlockHeaderHex(ctx, d, h1)
	lowFee := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":1}`, hx1))
	txO, sdO, ms := vfBuildSweep(t, d, ctx, 1, cid)
	if txO == "" || sdO == nil {
		t.Fatalf("PRECONDITION FAILED: no sweep built (addBlocks(fee=1)=%s migrateVault=%s)", lowFee, ms)
	}
	rawO, signedO := "", false
	for attempt := 1; attempt <= 3 && !signedO; attempt++ {
		rawO, signedO = vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sdO)
	}
	if !signedO {
		t.Fatalf("PRECONDITION FAILED: sweep %s never fully signed", txO)
	}
	bcO, berr := d.bitcoinCli(ctx, "sendrawtransaction", rawO)
	c.rec("F28-LOWFEE-UNCONFIRMED", "a 1 sat/vB sweep is built, signed and accepted into the regtest mempool, left unconfirmed",
		berr == nil && strings.EqualFold(strings.TrimSpace(bcO), txO) && f28InMempool(d, ctx, txO),
		fmt.Sprintf("addBlocks(fee=1)=%s txid=%s broadcast=%q err=%v in_mempool=%v", lowFee, txO, strings.TrimSpace(bcO), berr, f28InMempool(d, ctx, txO)))
	if berr != nil {
		finish()
		return
	}

	// ---- 2. age it past the redrive staleness at a HIGHER relayed fee, without mining O ----
	// Mining would confirm O; regtest lets us mine only blocks that exclude it by
	// generating to a template with no mempool: use generateblock with an empty tx list.
	minerAddr, _ := d.bitcoinCli(ctx, "getnewaddress")
	mined := 0
	for i := 0; i < 14; i++ {
		if _, err := d.bitcoinCli(ctx, "generateblock", minerAddr, `[]`); err != nil {
			t.Logf("generateblock (empty) failed: %v", err)
			break
		}
		mined++
	}
	tip, _ := d.BitcoinHeight(ctx)
	last := contractLastHeight(t, d, ctx, cid)
	for hh := last + 1; hh <= tip; hh++ {
		hx, herr := btcBlockHeaderHex(ctx, d, hh)
		if herr != nil {
			break
		}
		vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx))
	}
	stillO := f28InMempool(d, ctx, txO)
	t.Logf("aged %d empty blocks (tip %d, contract last %d); O still in mempool=%v", mined, tip, contractLastHeight(t, d, ctx, cid), stillO)

	// ---- 3. redrive: the contract builds R over the same inputs at the bumped fee ----
	before := txSpendIds(t, d, ctx, cid)
	rd := ""
	for attempt := 1; attempt <= 3; attempt++ {
		rd = vstatus(t, d, ctx, 1, cid, "redriveSpend", txO)
		if isOK(rd) {
			break
		}
		d.bitcoinCli(ctx, "generateblock", minerAddr, `[]`)
		tip, _ = d.BitcoinHeight(ctx)
		hx, _ := btcBlockHeaderHex(ctx, d, tip)
		vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx))
	}
	txR := ""
	for i := 0; i < 20 && isOK(rd) && txR == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(before, id) {
				txR = id
			}
		}
	}
	c.rec("F28-REDRIVE", "redriveSpend builds a replacement over the same inputs once the original is stale",
		isOK(rd) && txR != "", fmt.Sprintf("redriveSpend=%s replacement=%s", rd, txR))
	if txR == "" {
		finish()
		return
	}
	sdR := waitSigningData(t, d, ctx, cid, txR)
	rawR, signedR := "", false
	for attempt := 1; attempt <= 3 && sdR != nil && !signedR; attempt++ {
		rawR, signedR = vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sdR)
	}
	if !signedR {
		c.rec("F28-RBF-REPLACED", "the replacement is signed and REPLACES the original in the mempool (BIP-125)", false, "replacement never fully signed")
		finish()
		return
	}

	// ---- 4. BIP-125 replacement in the real mempool ----
	bcR, rerr := d.bitcoinCli(ctx, "sendrawtransaction", rawR)
	time.Sleep(3 * time.Second)
	rIn, oIn := f28InMempool(d, ctx, txR), f28InMempool(d, ctx, txO)
	c.rec("F28-RBF-REPLACED", "the replacement is signed and REPLACES the original in the mempool (BIP-125)",
		rerr == nil && rIn && !oIn,
		fmt.Sprintf("broadcast(R)=%q err=%v; mempool has R=%v O=%v", strings.TrimSpace(bcR), rerr, rIn, oIn))
	if rerr != nil {
		finish()
		return
	}

	// ---- 5. confirm R, settle the group, drain, unwedge NN#3 ----
	h, _ := d.MineBlocks(ctx, 1)
	_, rMined := f9TxBlockHash(d, ctx, txR)
	cs := vfRelayAndConfirmIndex(t, d, ctx, 1, cid, txR, h, 0)
	time.Sleep(15 * time.Second)
	msO, msR := vfSweepRecordOn(d, ctx, 2, cid, txO), vfSweepRecordOn(d, ctx, 2, cid, txR)
	gen0 := vfWaitGenBelow(t, d, ctx, cid, 0, 1, 3*time.Minute)
	c.rec("F28-CONFIRM-SETTLES-GROUP", "confirming the replacement settles the whole spend group (both records gone) and drains gen-0",
		rMined && isOK(cs) && !msO && !msR && gen0 == 0,
		fmt.Sprintf("R mined=%v confirmSpend=%s ms_O=%v ms_R=%v gen-0 utxos=%d", rMined, cs, msO, msR, gen0))

	ck := vstatus(t, d, ctx, 1, cid, "createKey", "")
	time.Sleep(10 * time.Second)
	gen2 := vfVaultStatusOn(d, ctx, 2, cid, 2)
	c.rec("F28-NN3-UNWEDGED", "with gen-0 drained the next createKey is admitted (NN#3 unwedged) and mints gen-2 as Pending",
		isOK(ck) && gen2 == 0, fmt.Sprintf("createKey=%s gen-2 status=%d (0=Pending)", ck, gen2))

	finish()
}

// f28InMempool reports whether txid is in bitcoind's mempool.
func f28InMempool(d *Devnet, ctx context.Context, txid string) bool {
	out, err := d.bitcoinCli(ctx, "getrawmempool")
	return err == nil && strings.Contains(out, txid)
}
