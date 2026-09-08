package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultWriteOffDust proves the VR2-11 fee-window deadlock.
//
// CORRECTION 2026-09-06: the min-deposit floor (constants.MinDepositSats = 1,000) is NOT
// unconditional. mapping.go:87 gates the sub-min skip on hasSupersededGen, so it is INERT
// until the first rotation: pre-rotation a sub-min deposit (above Bitcoin's own ~546-sat
// dust limit) IS credited. The earlier "unconditional floor" reading (from Stage7
// MD03-EDGE-07, whose 500-sat deposit is below Bitcoin dust and never reached the floor)
// was wrong; run 3 observed a 600-sat pre-rotation deposit credited. The write-off
// positive path is therefore REACHABLE (see TestVaultWriteOffDustOrphansBalance / VR2-15).
//
// The deadlock itself stands: the sweep builder defers a tranche when the fee at the
// CURRENT oracle rate exceeds half its value (migration.go:209), while writeOffDust judges
// dust at the MINIMUM rate (dust_writeoff.go:59). A 1,000-sat residual is sweepable at the
// minimum rate (1,000 - ~144 = 856 > 546) so write-off REFUSES it, yet it is deferred
// whenever fees are above ~3.5 sat/vB, so the retiring generation stays "funded":
// createKey (NN#3) and retireVault refuse, bonds stay locked, until fees fall.
//
//	WOD-00   the 1,000-sat deposit is credited pre-rotation (floor inert until rotation).
//	WOD-00b  post-rotation a 700-sat deposit to the active gen is SKIPPED (floor engaged).
//	WOD-01   at latest_fee 10 the sweep is ACCEPTED — the builder derives an affordable
//	         rate under the same ceiling rather than deferring (VR2-11 fixed).
//	WOD-02   writeOffDust REFUSES the same residual (not dust at the minimum rate).
//	WOD-03   gen-0 has moved off Retiring (a sweep is in flight); NN#3 still refuses a
//	         new rotation until the residual settles, which is correct.
//	WOD-04   one addBlocks with latest_fee 1 and the same sweep succeeds and settles.
//	WOD-05   retireVault then moves gen-0 to Inactive.
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

	// Pre-rotation (only gen-0 active) the min-deposit floor is inert (mapping.go:87 gates
	// on hasSupersededGen), so the 1,000-sat residual under test is credited normally.
	const residualSats = 1000
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, residualSats, seedH)
	n0 := 0
	for i := 0; i < 12 && n0 == 0; i++ {
		time.Sleep(10 * time.Second)
		n0 = genUtxoCount(t, d, ctx, cid, 0)
	}
	rec("WOD-00", "the 1,000-sat deposit is credited pre-rotation (min-deposit floor is inert until the first rotation)", n0 == 1,
		fmt.Sprintf("gen-0 holds %d UTXO(s), balance=%d", n0, balanceSats(t, d, ctx, cid, owner)))
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

	// ── WOD-00b: with gen-0 now Retiring (a superseded gen exists), the floor engages. ──
	// A 700-sat deposit (above Bitcoin dust, below MinDepositSats) to the active gen-1 is
	// skipped per-output: the map op succeeds but credits nothing.
	subBefore := genUtxoCount(t, d, ctx, cid, 1)
	fundVaultViaSPV(t, d, ctx, cid, primary1, backupPubKeyG, owner, 700, contractLastHeight(t, d, ctx, cid))
	time.Sleep(20 * time.Second)
	subAfter := genUtxoCount(t, d, ctx, cid, 1)
	rec("WOD-00b", "post-rotation the min-deposit floor engages: a 700-sat deposit to the active gen-1 is skipped (hasSupersededGen)", subAfter == subBefore,
		fmt.Sprintf("gen-1 UTXO count %d -> %d after a 700-sat deposit (want unchanged)", subBefore, subAfter))

	// ── WOD-01: migrateVault must REFUSE the all-dust residual (uneconomic). ──
	// WOD-01: at the devnet's relayed rate (latest_fee 10) the 1,000-sat tranche is
	// DEFERRED: fee ~1,410 sats > 500 (half the value).
	mig := vstatus(t, d, ctx, 1, cid, "migrateVault", "")
	time.Sleep(10 * time.Second)
	stillThere := genUtxoCount(t, d, ctx, cid, 0)
	// INVERTED BY VR2-11. This case used to assert the DEADLOCK: at latest_fee 10 the
	// 1,000-sat tranche priced at ~1,410 sats, more than half its value, so the sweep
	// was deferred while write-off correctly refused the same residual — and nothing
	// moved until fees fell, with NN#3 blocking every rotation meanwhile.
	//
	// The builder now derives the highest rate that fits under the SAME ceiling
	// instead of insisting on the oracle's, so the sweep proceeds. The ceiling itself
	// is unchanged: the fee still cannot exceed half the tranche. The UTXO stays on
	// gen-0 until confirmSpend settles it, so this asserts the sweep was ACCEPTED,
	// not that the generation has already drained.
	rec("WOD-01", "at latest_fee 10 the sweep of the 1,000-sat residual is ACCEPTED: the builder derives an affordable rate under the same fee ceiling instead of deferring (VR2-11)",
		isOK(mig),
		fmt.Sprintf("migrateVault status=%s (want accepted), gen-0 holds %d UTXO(s) (still 1 until confirmSpend settles)", mig, stillThere))

	// WOD-02: writeOffDust judges at the MINIMUM rate, where 1,000 sats IS sweepable, so it
	// is a NO-OP: the call succeeds ("nothing to write off") and LEAVES the residual — it
	// never destroys a residual that could still be swept. The signal is the EFFECT (the
	// residual stays), NOT the call status: writeOffDust does not FAIL when nothing
	// qualifies, so the earlier !isOK(wod) assertion was wrong (run 4: status CONFIRMED,
	// residual correctly left in place).
	wod := vstatus(t, d, ctx, 1, cid, "writeOffDust", "")
	time.Sleep(10 * time.Second)
	afterWod := genUtxoCount(t, d, ctx, cid, 0)
	rec("WOD-02", "writeOffDust does NOT clear the 1,000-sat residual (sweepable at the minimum rate, so write-off leaves it): the residual stays on gen-0 and the deadlock persists", afterWod == 1,
		fmt.Sprintf("writeOffDust status=%s (a no-op: nothing to write off at the minimum rate), gen-0 holds %d UTXO(s) (want 1, still stuck)", wod, afterWod))

	// WOD-03: nothing else can move it either: the deadlock.
	rv := vstatus(t, d, ctx, 1, cid, "retireVault", "")
	ck := vstatus(t, d, ctx, 1, cid, "createKey", "")
	time.Sleep(10 * time.Second)
	st := vaultStatusOf(t, d, ctx, cid, 0)
	gen2 := vaultStatusOf(t, d, ctx, cid, 2)
	// INVERTED BY VR2-11, but only partly. NN#3 still correctly refuses a new
	// rotation while gen-0 holds an unsettled UTXO — that guard is not what was
	// broken. What changed is WHY gen-0 still holds it: previously the sweep could
	// never be built at all, so the wait was unbounded and ended only when fees fell.
	// Now a sweep is in flight, so gen-0 has moved off Retiring and the residual
	// clears on settle.
	rec("WOD-03", "with the sweep in flight gen-0 is no longer parked in Retiring, and NN#3 still (correctly) refuses a new rotation until the residual settles",
		st != 2 && !isOK(ck) && gen2 < 0,
		fmt.Sprintf("retireVault=%s gen-0 status=%s (want Draining or beyond, NOT Retiring), createKey=%s (want refused while funds remain), gen-2 status=%d (want absent)", rv, statusStr(st), ck, gen2))

	// WOD-04 (on/off): drop the relayed fee rate to 1 sat/vB and the SAME sweep goes through.
	h, _ := d.MineBlocks(ctx, 1)
	hx, _ := btcBlockHeaderHex(ctx, d, h)
	ab := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":1}`, hx))
	migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
	after := vfWaitGenBelow(t, d, ctx, cid, 0, 1, 3*time.Minute)
	rec("WOD-04", "the residual sweeps and settles to completion (the fee-rate mismatch between the two gates no longer strands it)", isOK(ab) && after == 0,
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
