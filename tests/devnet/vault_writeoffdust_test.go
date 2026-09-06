package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultWriteOffDust, rewritten 2026-09-06 as the VR2-11 on/off measurement.
//
// The July version deposited 600 sats expecting the min-deposit floor to be OFF before
// the first rotation. PR #29's contract enforces MinDepositSats = 1,000 unconditionally
// (Stage7 MD03-EDGE-07), and with that floor a residual is ALWAYS sweepable at the
// minimum fee (1,000 - ~144 = 856 > 546), so isResidualUnsweepableAtMinFee can never be
// true on a fresh v2 deploy: the write-off positive path is unreachable by design.
//
// What IS reachable is worse. The sweep builder defers a tranche when the fee at the
// CURRENT oracle rate exceeds half its value (migration.go:209), while writeOffDust judges
// dust at the MINIMUM rate (dust_writeoff.go:59). A 1,000-sat residual is therefore
// deferred whenever fees are above ~3.5 sat/vB AND refused by write-off, so the retiring
// generation stays "funded": createKey (NN#3) and retireVault refuse, bonds stay locked,
// until fees fall. This test proves both halves:
//
//	WOD-00  the 600-sat deposit is refused (floor unconditional); 1,000 sats is credited.
//	WOD-01  at latest_fee 10 the sweep is DEFERRED (migrateVault not OK, residual stays).
//	WOD-02  writeOffDust REFUSES the same residual (not dust at the minimum rate).
//	WOD-03  retireVault leaves gen-0 Retiring and createKey is refused: the deadlock.
//	WOD-04  one addBlocks with latest_fee 1 and the same sweep succeeds and settles.
//	WOD-05  retireVault then moves gen-0 to Inactive.
//
//	VAULT_WOD_RUN=1 go test -v -run TestVaultWriteOffDust -timeout 45m ./tests/devnet/
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
	// VR2-09: let the post-DKG pre-parameter regeneration finish before the check-sig.
	vfWaitPreparams(t, d, ctx, 12*time.Minute)
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary0, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen0 register: %s", s)
	}
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)

	// 600 sats: at the fixed 1 sat/vByte floor a 1-input sweep costs ~144 sat, leaving
	// 456 <= the 546 dustThreshold — provably un-sweepable at ANY fee rate.
	// The floor is unconditional: a 600-sat deposit maps but is not credited.
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 600, seedH)
	time.Sleep(10 * time.Second)
	nSub := genUtxoCount(t, d, ctx, cid, 0)
	// The smallest accepted deposit is the residual under test.
	const residualSats = 1000
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, residualSats, contractLastHeight(t, d, ctx, cid))
	n0 := 0
	for i := 0; i < 12 && n0 == 0; i++ {
		time.Sleep(10 * time.Second)
		n0 = genUtxoCount(t, d, ctx, cid, 0)
	}
	rec("WOD-00", "the 600-sat deposit is refused (min-deposit floor is unconditional) and the 1,000-sat minimum is credited", nSub == 0 && n0 == 1,
		fmt.Sprintf("after 600 sats gen-0 held %d UTXO(s); after 1,000 sats gen-0 holds %d, balance=%d", nSub, n0, balanceSats(t, d, ctx, cid, owner)))
	if n0 != 1 {
		t.Logf("WOD SUMMARY: %d PASS %d FAIL", pass, fail)
		return
	}

	// ── rotate to gen-1 ──
	// Hardened 2026-09-05: the 8-minute log-and-continue wait let the test run with v2 OFF
	// under load (observed: node at block 292 after 8m with hpin=400) and produced vacuous
	// v2 claims. vfWaitV2On waits 18 minutes on every node and is fatal on a miss.
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

	// Fund a fee reserve so a FAILED migrateVault can only be due to dustiness, not a
	// missing reserve (the reserve funds gen-1, not gen-0).
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ── WOD-01: migrateVault must REFUSE the all-dust residual (uneconomic). ──
	// WOD-01: at the devnet's relayed rate (latest_fee 10) the 1,000-sat tranche is
	// DEFERRED: fee ~1,410 sats > 500 (half the value).
	mig := vstatus(t, d, ctx, 1, cid, "migrateVault", "")
	time.Sleep(10 * time.Second)
	stillThere := genUtxoCount(t, d, ctx, cid, 0)
	rec("WOD-01", "at latest_fee 10 the sweep of the 1,000-sat residual is DEFERRED (fee > half the tranche) and the residual stays on gen-0", !isOK(mig) && stillThere == 1,
		fmt.Sprintf("migrateVault status=%s (want refused), gen-0 holds %d UTXO(s)", mig, stillThere))

	// WOD-02: writeOffDust judges at the MINIMUM rate, where 1,000 sats IS sweepable, so it refuses.
	wod := vstatus(t, d, ctx, 1, cid, "writeOffDust", "")
	time.Sleep(10 * time.Second)
	afterWod := genUtxoCount(t, d, ctx, cid, 0)
	rec("WOD-02", "writeOffDust REFUSES the same residual (sweepable at the minimum rate: 1,000 - ~144 > 546)", !isOK(wod) && afterWod == 1,
		fmt.Sprintf("writeOffDust status=%s (want refused), gen-0 holds %d UTXO(s)", wod, afterWod))

	// WOD-03: nothing else can move it either: the deadlock.
	rv := vstatus(t, d, ctx, 1, cid, "retireVault", "")
	ck := vstatus(t, d, ctx, 1, cid, "createKey", "")
	time.Sleep(10 * time.Second)
	st := vaultStatusOf(t, d, ctx, cid, 0)
	gen2 := vaultStatusOf(t, d, ctx, cid, 2)
	rec("WOD-03", "retireVault leaves gen-0 Retiring and createKey is refused (NN#3): a 1,000-sat residual freezes the rotation while fees are high",
		st == 2 && !isOK(ck) && gen2 < 0,
		fmt.Sprintf("retireVault=%s gen-0 status=%s (want Retiring), createKey=%s (want refused), gen-2 status=%d (want absent)", rv, statusStr(st), ck, gen2))

	// WOD-04 (on/off): drop the relayed fee rate to 1 sat/vB and the SAME sweep goes through.
	h, _ := d.MineBlocks(ctx, 1)
	hx, _ := btcBlockHeaderHex(ctx, d, h)
	ab := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":1}`, hx))
	migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
	after := vfWaitGenBelow(t, d, ctx, cid, 0, 1, 3*time.Minute)
	rec("WOD-04", "with latest_fee 1 the identical residual sweeps and settles (the freeze is purely the fee-rate mismatch between the two gates)", isOK(ab) && after == 0,
		fmt.Sprintf("addBlocks(latest_fee=1)=%s, gen-0 holds %d UTXO(s) after the sweep", ab, after))

	if after == 0 {
		vstatus(t, d, ctx, 1, cid, "retireVault", "")
		st2 := -1
		for i := 0; i < 8; i++ {
			st2 = vaultStatusOf(t, d, ctx, cid, 0)
			if st2 == 4 {
				break
			}
			time.Sleep(10 * time.Second)
		}
		rec("WOD-05", "gen-0 retires (Inactive) once the residual is gone", st2 == 4, "gen-0 status="+statusStr(st2))
	}

	t.Logf("WOD SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}
