package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultWriteOffDustOrphansBalance proves VR2-15: a sub-MinDepositSats deposit made to
// a v2 vault BEFORE its first rotation is credited to the depositor's L2 balance (the
// min-deposit floor is inert until a superseded generation exists, mapping.go:87), but
// after rotation that lone residual can be written off by the owner-only writeOffDust,
// which deletes the UTXO and debits ActiveSupply+UserSupply WITHOUT clearing the
// depositor's per-account balance (dust_writeoff.go:162-176). The depositor is then left
// with a balance the vault can no longer back: the withdrawal fails at UTXO selection
// (unmapping.go:278) and the UserSupply == Σ(a-*) invariant is broken. The written-off
// amount is bounded at < MinDepositSats (1,000 sats) per such deposit, and writeOffDust is
// owner-only, so this is LOW severity — but it is a real fault (credited funds whose
// backing is destroyed) and it also demonstrates that the write-off positive path is
// REACHABLE, correcting the earlier "unreachable by design" reading.
//
// The horns are exclusive and both bad: if the owner does NOT write off the lone residual
// it can never be swept economically (below dust after fee even at rate 1), so gen-0 never
// empties and the rotation cannot complete (NN#3 / bond lock) — the VR2-01/VR2-11 family.
//
//	ORPH-00  a 600-sat pre-rotation deposit is credited (balance 600, gen-0 holds 1 UTXO).
//	ORPH-01  after rotation the lone 600 residual cannot be swept even at latest_fee 1
//	         (600 - ~144 = 456 <= the 546 dust threshold, migrateVault aborts).
//	ORPH-02  writeOffDust force-retires the residual (positive path REACHABLE): UTXO gone.
//	ORPH-03  the depositor's L2 balance SURVIVES the write-off (still 600) — the fault.
//	ORPH-04  the orphaned balance cannot be withdrawn (unmap fails, no UTXO) — the damage.
//
// A GREEN run means the bug is reproduced (ORPH-03/04 assert the faulty state). Flip those
// expectations when VR2-15 is fixed (write-off must also debit the crediting account, or
// the floor must apply pre-rotation too).
//
//	VAULT_WOD_ORPHAN_RUN=1 go test -v -run TestVaultWriteOffDustOrphansBalance -timeout 45m ./tests/devnet/
func TestVaultWriteOffDustOrphansBalance(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_WOD_ORPHAN_RUN") == "" {
		t.Skip("set VAULT_WOD_ORPHAN_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		t.Fatal("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm")
	}

	const hpin = 400
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 38*time.Minute)

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "writeoff-orphan", DeployerNode: 1, GQLNode: 2,
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

	// ── gen-0 keygen + register ──
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("gen0 keygen: %v", err)
	}
	primary0 := kd0.PublicKey
	vfWaitPreparams(t, d, ctx, 12*time.Minute) // VR2-09
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary0, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen0 register: %s", s)
	}
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)

	// ── ORPH-00: a lone sub-min (600) deposit is credited pre-rotation. ──
	// 600 is above Bitcoin's ~546-sat dust (so bitcoind mines it) and below MinDepositSats
	// (1,000), and the floor is inert pre-rotation.
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 600, seedH)
	n0 := 0
	for i := 0; i < 12 && n0 == 0; i++ {
		time.Sleep(10 * time.Second)
		n0 = genUtxoCount(t, d, ctx, cid, 0)
	}
	bal0 := balanceSats(t, d, ctx, cid, owner)
	rec("ORPH-00", "a 600-sat pre-rotation deposit is credited (min-deposit floor inert until rotation)", n0 == 1 && bal0 == 600,
		fmt.Sprintf("gen-0 holds %d UTXO(s), balance(%s)=%d", n0, owner, bal0))
	if n0 != 1 || bal0 != 600 {
		t.Logf("WOD-ORPHAN SUMMARY: %d PASS %d FAIL", pass, fail)
		return
	}

	// ── rotate to gen-1 (gen-0 becomes Retiring with the lone 600 residual). ──
	vfWaitV2On(t, d, ctx, uint64(hpin))
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
	activated := false
	for i := 0; i < 20; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
			activated = true
			break
		}
		time.Sleep(15 * time.Second)
	}
	if !activated {
		t.Fatal("gen-1 never activated")
	}
	if st := vaultStatusOf(t, d, ctx, cid, 0); st != 2 {
		t.Fatalf("PRECONDITION: gen-0 status=%s (want Retiring) after rotation", statusStr(st))
	}
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ── ORPH-01: the lone 600 residual cannot be swept even at the minimum fee rate. ──
	h, _ := d.MineBlocks(ctx, 1)
	hx, _ := btcBlockHeaderHex(ctx, d, h)
	vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":1}`, hx))
	mig := vstatus(t, d, ctx, 1, cid, "migrateVault", "")
	time.Sleep(15 * time.Second)
	afterMig := genUtxoCount(t, d, ctx, cid, 0)
	rec("ORPH-01", "the lone 600-sat residual cannot be swept even at latest_fee 1 (below dust after fee)", afterMig == 1,
		fmt.Sprintf("migrateVault status=%s, gen-0 holds %d UTXO(s) (want 1, still stuck)", mig, afterMig))

	// ── ORPH-02: writeOffDust force-retires it (positive path REACHABLE). ──
	wod := vstatus(t, d, ctx, 1, cid, "writeOffDust", "")
	afterWod := 1
	for i := 0; i < 12 && afterWod != 0; i++ {
		time.Sleep(10 * time.Second)
		afterWod = genUtxoCount(t, d, ctx, cid, 0)
	}
	rec("ORPH-02", "writeOffDust force-retires the lone sub-min residual (write-off positive path is REACHABLE)", isOK(wod) && afterWod == 0,
		fmt.Sprintf("writeOffDust status=%s, gen-0 holds %d UTXO(s) (want 0)", wod, afterWod))

	// ── ORPH-03: the depositor's balance SURVIVES the write-off (the fault). ──
	balAfter := balanceSats(t, d, ctx, cid, owner)
	rec("ORPH-03", "the depositor's L2 balance survives the write-off (Supply+UTXO destroyed, a-account NOT cleared) — VR2-15 fault", balAfter == 600,
		fmt.Sprintf("balance(%s)=%d after write-off (still 600 = orphaned; write-off debited Supply and deleted the UTXO but never cleared the account)", owner, balAfter))

	// ── ORPH-04: the orphaned balance cannot be withdrawn (the damage). ──
	dest, _ := d.bitcoinCli(ctx, "getnewaddress")
	um := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, 600, dest))
	rec("ORPH-04", "the orphaned 600 balance cannot be withdrawn (no UTXO backs it): user fund loss + broken UserSupply invariant — VR2-15 damage", !isOK(um),
		fmt.Sprintf("unmap(600) status=%s (want refused), balance still=%d", um, balanceSats(t, d, ctx, cid, owner)))

	t.Logf("WOD-ORPHAN SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}
