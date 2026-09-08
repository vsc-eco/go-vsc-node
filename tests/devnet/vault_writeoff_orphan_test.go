package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultWriteOffDustOrphansBalance is the devnet regression test for VR2-15 (FIXED). A sub-MinDepositSats deposit made to
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
//	ORPH-03  the depositor keeps their credit AND UserSupply is untouched (I3 exact).
//	ORPH-04  the fee reserve is debited by EXACTLY the written-off amount.
//
// ★ THIS TEST WAS INVERTED AND HAS BEEN TURNED AROUND. It used to assert the FAULT: a GREEN
// run meant VR2-15 was reproduced, with ORPH-03/04 encoding the orphaned balance and the
// failed withdrawal. VR2-15 is fixed, so it now asserts the FIXED state instead.
//
// The fix took a different route than this file anticipated. The old note said to flip these
// by making the write-off "also debit the crediting account" -- a clawback that needs a
// per-UTXO recipient and can still FAIL when the depositor has already moved the credit, at
// which point the write-off either reverts (leaving NN#3 wedged, the V-1 deadlock) or forces
// a negative balance. Instead the residual is charged to the operator's fee reserve, which
// this contract already uses for rotation costs that must not touch principal. So the
// depositor KEEPING their balance is now the correct outcome, and what has to be proven is
// that the sats came out of the reserve -- which is what ORPH-04 measures, as an exact
// equality rather than a direction.
//
// Note also that ORPH-04's old assertion would still PASS against the fixed contract, for
// the wrong reason: unmap(600) is refused either way, because a 600-sat balance cannot cover
// 600 plus the fee. A test that passes under both the bug and the fix measures neither.
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
	supBefore := vf11ReadSupply(d, ctx, 2, cid)
	wod := vstatus(t, d, ctx, 1, cid, "writeOffDust", "")
	afterWod := 1
	for i := 0; i < 12 && afterWod != 0; i++ {
		time.Sleep(10 * time.Second)
		afterWod = genUtxoCount(t, d, ctx, cid, 0)
	}
	rec("ORPH-02", "writeOffDust force-retires the lone sub-min residual (write-off positive path is REACHABLE)", isOK(wod) && afterWod == 0,
		fmt.Sprintf("writeOffDust status=%s, gen-0 holds %d UTXO(s) (want 0)", wod, afterWod))

	// ── ORPH-03: the depositor keeps the credit the protocol still owes them, and
	// UserSupply still stands for exactly the balances that exist (VR2-15 FIXED). ──
	//
	// This case used to assert the FAULT: the balance survived while the write-off
	// debited UserSupply out from under it, so Sigma(balances) exceeded UserSupply
	// forever. That is not cosmetic -- HandleUnmap decrements UserSupply with
	// safeSubtract64 on EVERY withdrawal, so once the aggregate ran short of the
	// balances it stood for, the LAST withdrawers underflowed and their withdrawals
	// reverted permanently, hitting whoever happened to withdraw last rather than the
	// depositor whose dust caused it.
	//
	// The write-off is now charged to the operator's fee reserve, so the balance
	// surviving is CORRECT rather than orphaned: it is still backed.
	supAfter := vf11ReadSupply(d, ctx, 2, cid)
	balAfter := balanceSats(t, d, ctx, cid, owner)
	rec("ORPH-03", "the depositor keeps their credit AND UserSupply is untouched, so Sigma(balances) == UserSupply stays exact (VR2-15 fixed)",
		balAfter == 600 && supBefore.readable && supAfter.readable &&
			supAfter.user == supBefore.user && supAfter.active == supBefore.active,
		fmt.Sprintf("balance(%s)=%d (want 600); user %d -> %d, active %d -> %d (both want UNCHANGED)",
			owner, balAfter, supBefore.user, supAfter.user, supBefore.active, supAfter.active))

	// ── ORPH-04: the RESERVE absorbed it, by exactly the written-off amount. ──
	//
	// This is the assertion that distinguishes the fix from merely deleting the debit:
	// the sats have to come from somewhere, and "somewhere" must be the operator's
	// reserve, not user principal and not thin air. Conservation is checked as an
	// equality on the fee delta, not as "fee went down".
	feeDelta := supBefore.fee - supAfter.fee
	rec("ORPH-04", "the fee reserve absorbed the write-off, debited by EXACTLY the residual (600) -- rotation cost, never user principal",
		supBefore.readable && supAfter.readable && feeDelta == 600,
		fmt.Sprintf("fee %d -> %d (delta %d, want exactly 600); active %d, user %d, balance %d",
			supBefore.fee, supAfter.fee, feeDelta, supAfter.active, supAfter.user, balAfter))

	t.Logf("WOD-ORPHAN SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}
