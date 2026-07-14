package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultWriteOffDust proves the deadlock ESCAPE HATCH end-to-end: a retiring
// generation whose only residual is provably un-sweepable (a sub-dust UTXO that
// cannot cover its own miner fee) can be force-retired via writeOffDust, so the
// drain reaches zero and the generation retires. Without this, that residual
// would pin the generation "funded" forever and rotation would stall permanently.
//
// The sub-dust deposit is credited because the min-deposit floor is gated on
// hasSupersededGen — it engages only AFTER the first rotation — so a pre-rotation
// deposit below the floor is credited to gen-0, then becomes an un-sweepable
// residual once gen-0 is superseded. This is exactly the real mainnet path.
//
//	VAULT_WOD_RUN=1 go test -v -run TestVaultWriteOffDust -timeout 42m ./tests/devnet/
func TestVaultWriteOffDust(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_WOD_RUN") == "" {
		t.Skip("set VAULT_WOD_RUN=1")
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

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "writeoff-dust", DeployerNode: 1, GQLNode: 2,
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

	// ── genesis gen-0, then deposit a SUB-DUST amount (floor is off pre-rotation). ──
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

	// 600 sats: at the fixed 1 sat/vByte floor a 1-input sweep costs ~144 sat, leaving
	// 456 <= the 546 dustThreshold — provably un-sweepable at ANY fee rate.
	const dustSats = 600
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, dustSats, seedH)
	n0 := genUtxoCount(t, d, ctx, cid, 0)
	rec("WOD-00", "sub-dust deposit credited to gen-0 (floor off pre-rotation)", n0 == 1,
		fmt.Sprintf("gen-0 holds %d UTXO(s), balance=%d", n0, balanceSats(t, d, ctx, cid, owner)))
	if n0 != 1 {
		t.Logf("WOD SUMMARY: %d PASS %d FAIL", pass, fail)
		return
	}

	// ── rotate to gen-1 ──
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
	activated := false
	for i := 0; i < 12; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
			activated = true
			break
		}
		time.Sleep(15 * time.Second)
	}
	if !activated {
		t.Fatal("gen-1 never activated")
	}

	// Fund a fee reserve so a FAILED migrateVault can only be due to dustiness, not a
	// missing reserve (the reserve funds gen-1, not gen-0).
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ── WOD-01: migrateVault must REFUSE the all-dust residual (uneconomic). ──
	mig := vstatus(t, d, ctx, 1, cid, "migrateVault", "")
	rec("WOD-01", "migrateVault refuses the un-sweepable dust residual", !isOK(mig), "status="+mig)
	stillDust := genUtxoCount(t, d, ctx, cid, 0)
	rec("WOD-01b", "the dust residual is still on gen-0 after the refused sweep", stillDust == 1,
		fmt.Sprintf("gen-0 holds %d UTXO(s)", stillDust))

	// ── WOD-02: writeOffDust force-retires the residual. ──
	wod := vstatus(t, d, ctx, 1, cid, "writeOffDust", "")
	rec("WOD-02", "writeOffDust accepted", isOK(wod), "status="+wod)

	// ── WOD-03: gen-0 now holds NOTHING (the escape hatch actually drained it). Poll:
	// the writeOffDust output commits a beat after the tx confirms, and genUtxoCount
	// reads a possibly-behind node — a single read races the commit (a unit test with the
	// identical 600-sat/BaseFeeRate=10 scenario proves the contract deletes it). ──
	after := 1
	for i := 0; i < 8; i++ {
		after = genUtxoCount(t, d, ctx, cid, 0)
		if after == 0 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	rec("WOD-03", "gen-0 drained to zero via writeOffDust", after == 0,
		fmt.Sprintf("gen-0 holds %d UTXO(s)", after))

	// ── WOD-04: with the residual gone, retire advances gen-0 to Inactive. ──
	if after == 0 {
		vstatus(t, d, ctx, 1, cid, "retireVault", "")
		st := -1
		for i := 0; i < 8; i++ {
			st = vaultStatusOf(t, d, ctx, cid, 0)
			if st == 4 {
				break
			}
			time.Sleep(10 * time.Second)
		}
		rec("WOD-04", "gen-0 retired (Inactive) once the dust was written off", st == 4, "gen-0 status="+statusStr(st))
	}

	t.Logf("WOD SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}
