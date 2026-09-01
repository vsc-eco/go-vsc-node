package devnet

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"go.mongodb.org/mongo-driver/bson"
)

// sweepOutputValue reads the (unsigned) sweep tx from the contract's signing data
// for txId and returns its first output value (sats to the successor). Lower value
// == higher miner fee for the same inputs — how we observe a re-drive fee bump.
func sweepOutputValue(t *testing.T, d *Devnet, ctx context.Context, cid, txId string) int64 {
	t.Helper()
	sd := waitSigningData(t, d, ctx, cid, txId)
	if sd == nil {
		return -1
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		t.Logf("sweepOutputValue: deser %s: %v", txId, err)
		return -1
	}
	if len(tx.TxOut) == 0 {
		return -1
	}
	return tx.TxOut[0].Value
}

// TestVaultRedriveSweep proves the L7-01 re-drive at the CONTRACT level: a stuck,
// never-confirming migration sweep can be replaced by a higher-fee sweep once it is
// stale, the staleness gate refuses a premature re-drive, and the op is scoped to the
// operator (not a stranger). This is the mechanism that unwedges a rotation whose sweep
// got stuck at too low a fee — without it, a low-fee sweep pins the generation forever.
//
// It exercises the contract logic (staleness clock, replacement build, fee bump, spend
// group, operator auth) without depending on real bitcoind RBF mempool eviction: the
// sweep is BUILT but never broadcast, so the contract's BuildHeight-vs-LastHeight clock —
// the thing under test — is driven purely by relaying headers.
//
//	VAULT_REDRIVE_RUN=1 go test -v -run TestVaultRedriveSweep -timeout 42m ./tests/devnet/
func TestVaultRedriveSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_REDRIVE_RUN") == "" {
		t.Skip("set VAULT_REDRIVE_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		t.Fatal("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm")
	}

	const hpin = 400
	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 38*time.Minute)

	operator := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 2)
	stranger := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 3)

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "redrive", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s", cid)
	vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH))
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	time.Sleep(10 * time.Second)

	pass, fail := 0, 0
	rec := func(id, desc string, ok bool, detail string) {
		if ok {
			pass++
			t.Logf("CASE %s PASS — %s | %s", id, desc, detail)
		} else {
			fail++
			t.Errorf("CASE %s FAIL — %s | %s", id, desc, detail)
		}
	}

	// ── genesis gen-0, fund with one normal (sweepable) UTXO ──
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("gen0 keygen: %v", err)
	}
	primary0 := kd0.PublicKey
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary0, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen0 register: %s", s)
	}
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 50_000_000, seedH)

	// ── rotate to gen-1 + fund the fee reserve ──
	if err := d.WaitForBlockProcessing(ctx, 2, hpin+5, 8*time.Minute); err != nil {
		t.Logf("wait hpin: %v", err)
	}
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd1, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-mainv1", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("gen1 keygen: %v", err)
	}
	primary1 := kd1.PublicKey
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary1, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen1 register: %s", s)
	}
	for i := 0; i < 12; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
			break
		}
		time.Sleep(15 * time.Second)
	}
	vstatus(t, d, ctx, 1, cid, "setVaultOperator", operator)
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ── build stuck sweep A (do NOT sign/broadcast: it stays unconfirmed forever). ──
	before := txSpendIds(t, d, ctx, cid)
	if s := vstatus(t, d, ctx, 1, cid, "migrateVault", ""); !isOK(s) {
		t.Fatalf("migrateVault (build sweep A): %s", s)
	}
	var txA string
	for i := 0; i < 20 && txA == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(before, id) {
				txA = id
				break
			}
		}
	}
	rec("RD-00", "migrateVault built a stuck sweep A", txA != "", "txA="+txA)
	if txA == "" {
		t.Logf("REDRIVE SUMMARY: %d PASS %d FAIL", pass, fail)
		return
	}
	valA := sweepOutputValue(t, d, ctx, cid, txA)

	// ── RD-01: re-drive is REFUSED while the sweep is still fresh (staleness gate). ──
	early := vstatus(t, d, ctx, 1, cid, "redriveSpend", txA)
	rec("RD-01", "premature re-drive refused (not yet stale)", !isOK(early), "status="+early)

	// ── age the sweep: relay RedriveStaleBlocks+1 (=13) empty headers so the contract's
	// LastHeight advances past the staleness window WITHOUT confirming sweep A. ──
	last := contractLastHeight(t, d, ctx, cid)
	h, _ := d.MineBlocks(ctx, 14)
	for hh := last + 1; hh <= h; hh++ {
		hx, _ := btcBlockHeaderHex(ctx, d, hh)
		vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx))
	}

	// ── RD-02: a STRANGER cannot re-drive (operator-scoped). ──
	strangerRD := vstatus(t, d, ctx, 3, cid, "redriveSpend", txA)
	rec("RD-02", "stranger refused redriveSpend (operator-scoped)", !isOK(strangerRD), "status="+strangerRD)
	_ = stranger

	// ── RD-03: the OPERATOR re-drives the now-stale sweep → a replacement B appears. ──
	beforeRD := txSpendIds(t, d, ctx, cid)
	opRD := vstatus(t, d, ctx, 2, cid, "redriveSpend", txA)
	rec("RD-03", "operator re-drove the stale sweep", isOK(opRD), "status="+opRD)
	var txB string
	for i := 0; i < 20 && txB == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if id != txA && !contains(beforeRD, id) {
				txB = id
				break
			}
		}
	}
	rec("RD-04", "a higher-fee replacement sweep B was created", txB != "", "txB="+txB)

	// ── RD-05: B pays a higher miner fee than A (same input, smaller successor output). ──
	if txB != "" {
		valB := sweepOutputValue(t, d, ctx, cid, txB)
		rec("RD-05", "replacement B pays MORE fee than A (output B < output A)", valB > 0 && valB < valA,
			fmt.Sprintf("outputA=%d outputB=%d (fee bump=%d sats)", valA, valB, valA-valB))
	}

	t.Logf("REDRIVE SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}
