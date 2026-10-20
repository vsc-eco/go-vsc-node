package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestVaultShadowEmptyGen drives two real rotations with generation 0 never funded, the
// SHADOW finding's worst form, and proves the fix end to end on a live network.
//
// Before the fix, migrateVault targeted the FIRST Retiring vault even when it held
// nothing, so the empty generation 0 shadowed the funded generation 1: every call
// returned "nothing to migrate for generation 0" and generation 1 never drained. And an
// empty Retiring generation had no way out of Retiring, so it was never purged and its
// committee's consensus bonds stayed locked (the lock is status-based).
//
// Cases:
//
//  1. SHADOW-EMPTY-RETIRING: after rotation 1, generation 0 is Retiring and holds nothing.
//
//  2. SHADOW-NN3: NN#3 counts funds, so the second rotation is admitted.
//
//  3. SHADOW-RESHARED: precondition, generation 2's key has had its first reshare.
//
//  4. SHADOW-DRAIN: generation 1 drains completely past the empty generation 0, which
//     migrate leaves Retiring.
//
//  5. SHADOW-R1-INACTIVE: retireVault moves both emptied generations to Inactive.
//
//  6. SHADOW-LATE-REVERT / SHADOW-LATE-SWEPT: a late deposit to generation 0's old
//     address re-funds it, retireVault reverts it to Draining, and it is swept.
//
//  7. SHADOW-PURGED: past the 144-block grace both reach Purged, which releases the bond
//     lock (under the POA exit-halt the unstake itself is not judged, see F24).
//
//  8. SHADOW-IDENT: the vault state is byte-identical across all 5 nodes.
//
//     VAULT_SHADOW_RUN=1 BTC_MAPPING_WASM_PATH=... go test -v -run TestVaultShadowEmptyGen -timeout 130m ./tests/devnet/
func TestVaultShadowEmptyGen(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_SHADOW_RUN") == "" {
		t.Skip("set VAULT_SHADOW_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := devnetWithTimeout(context.Background(), vfTestBudget(120*time.Minute))
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		t.Fatal("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract v2 regtest wasm")
	}

	const hpin = 400
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 120*time.Minute)
	c := &vfCase{t: t}

	env := vfSetup(t, d, ctx, wasm, hpin, 0, "shadow: empty generation")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)
	finish := func() {
		vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "SHADOW-IDENT")
		c.summary("SHADOW")
	}
	statusOn := func(gen uint32, want int, within time.Duration) int {
		st := vfVaultStatusOn(d, ctx, 2, cid, gen)
		for end := devnetNow().Add(within); st != want && devnetNow().Before(end); devnetSleep(10 * time.Second) {
			st = vfVaultStatusOn(d, ctx, 2, cid, gen)
		}
		return st
	}

	// 1. Rotation 1 with generation 0 empty.
	primary1, ok := vfRotate(t, d, ctx, cid, 1)
	if !ok {
		t.Fatalf("PRECONDITION FAILED: generation 1 never activated")
	}
	st0 := statusOn(0, 2, 90*time.Second)
	g0 := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	c.rec("SHADOW-EMPTY-RETIRING", "generation 0 retires holding nothing", st0 == 2 && g0 == 0,
		fmt.Sprintf("gen0 status=%s utxos=%d", statusStr(st0), g0))

	// Fund generation 1 with two deposits above the 1M-sat canary: one canary tranche,
	// then one bulk tranche.
	fundVaultViaSPV(t, d, ctx, cid, primary1, backupPubKeyG, env.owner, 60_000_000, contractLastHeight(t, d, ctx, cid))
	fundVaultViaSPV(t, d, ctx, cid, primary1, backupPubKeyG, env.owner, 40_000_000, contractLastHeight(t, d, ctx, cid))
	g1 := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
	for end := devnetNow().Add(3 * time.Minute); g1 < 2 && devnetNow().Before(end); devnetSleep(10 * time.Second) {
		g1 = vfGenUtxoCountOn(d, ctx, 2, cid, 1)
	}
	if g1 < 2 {
		vfDumpRegistry(t, d, ctx, 2, cid, "generation 1 funding")
		t.Fatalf("PRECONDITION FAILED: generation 1 holds %d UTXOs, want 2", g1)
	}

	// 2. Rotation 2: NN#3 sees no funded superseded generation and admits it.
	primary2, ok := vfRotate(t, d, ctx, cid, 2)
	c.rec("SHADOW-NN3", "the second rotation is admitted while the superseded generation 0 is empty", ok,
		fmt.Sprintf("gen0 status=%s gen1 status=%s", statusStr(vfVaultStatusOn(d, ctx, 2, cid, 0)), statusStr(vfVaultStatusOn(d, ctx, 2, cid, 1))))
	if !ok {
		finish()
		return
	}
	fundFeeReserve(t, d, ctx, cid, primary2, backupPubKeyG, 10_000_000)

	// 3. migrateVault waits for the new key's first reshare; wait for it here so the
	// drain below measures the selection, not the reshare gate.
	key2 := cid + "-mainv2"
	reshared := false
	for end := devnetNow().Add(vfTestBudget(20 * time.Minute)); devnetNow().Before(end); devnetSleep(20 * time.Second) {
		if e, kg, found := custodyEpochs(d, ctx, 2, key2); found && e > kg {
			reshared = true
			t.Logf("generation 2 reshared: epoch %d > keygen %d", e, kg)
			break
		}
	}
	c.rec("SHADOW-RESHARED", "precondition: generation 2's key has had its first reshare", reshared, "")
	if !reshared {
		finish()
		return
	}

	// 4. Generation 1 drains past the empty generation 0.
	tranches := 0
	for i := 0; i < 4; i++ {
		left := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
		if left == 0 {
			break
		}
		migrateAndSettle(t, d, ctx, cid, cid+"-mainv1", primary2, backupPubKeyG)
		tranches++
		vfDumpRegistry(t, d, ctx, 2, cid, fmt.Sprintf("after generation 1 tranche %d", tranches))
		if after := vfWaitGenBelow(t, d, ctx, cid, 1, left, 3*time.Minute); after >= left {
			t.Logf("tranche %d made no progress (%d -> %d)", tranches, left, after)
			break
		}
		fundFeeReserve(t, d, ctx, cid, primary2, backupPubKeyG, 10_000_000)
	}
	g1Left := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
	st0 = vfVaultStatusOn(d, ctx, 2, cid, 0)
	c.rec("SHADOW-DRAIN", "generation 1 drains completely past the empty generation 0, which migrate leaves Retiring",
		g1Left == 0 && st0 == 2,
		fmt.Sprintf("%d tranche(s); gen1 utxos left=%d; gen0 status=%s", tranches, g1Left, statusStr(st0)))
	if g1Left != 0 {
		finish()
		return
	}

	// 5. R1: retireVault moves both emptied generations to Inactive.
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	st0 = statusOn(0, 4, 2*time.Minute)
	st1 := statusOn(1, 4, 2*time.Minute)
	c.rec("SHADOW-R1-INACTIVE", "retireVault moves the empty Retiring generation 0 and the drained generation 1 to Inactive",
		st0 == 4 && st1 == 4, fmt.Sprintf("gen0 status=%s gen1 status=%s", statusStr(st0), statusStr(st1)))

	// 6. Failure state: a late deposit to generation 0's old address.
	fundVaultViaSPV(t, d, ctx, cid, env.primary0, backupPubKeyG, env.owner, 10_000_000, contractLastHeight(t, d, ctx, cid))
	g0 = vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	for end := devnetNow().Add(3 * time.Minute); g0 < 1 && devnetNow().Before(end); devnetSleep(10 * time.Second) {
		g0 = vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	}
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	st0 = statusOn(0, 3, 2*time.Minute)
	c.rec("SHADOW-LATE-REVERT", "a late deposit to the Inactive generation 0 is credited to it and retireVault reverts it to Draining",
		g0 == 1 && st0 == 3, fmt.Sprintf("gen0 utxos=%d status=%s", g0, statusStr(st0)))
	if st0 == 3 {
		fundFeeReserve(t, d, ctx, cid, primary2, backupPubKeyG, 10_000_000)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary2, backupPubKeyG)
	}
	g0 = vfWaitGenBelow(t, d, ctx, cid, 0, 1, 3*time.Minute)
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	st0 = statusOn(0, 4, 2*time.Minute)
	c.rec("SHADOW-LATE-SWEPT", "the late deposit is swept to generation 2 and generation 0 returns to Inactive",
		g0 == 0 && st0 == 4, fmt.Sprintf("gen0 utxos=%d status=%s", g0, statusStr(st0)))

	// 7. Purge both after the grace window (mine 150 blocks, relay their headers).
	last := contractLastHeight(t, d, ctx, cid)
	h, merr := d.MineBlocks(ctx, 150)
	if merr != nil {
		t.Errorf("mining the purge grace window: %v", merr)
	}
	const relayBatch = 25
	for start := last + 1; start <= h; start += relayBatch {
		var hexBatch string
		for hh := start; hh < start+relayBatch && hh <= h; hh++ {
			hx, _ := btcBlockHeaderHex(ctx, d, hh)
			hexBatch += hx
		}
		if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hexBatch)); !isOK(s) {
			t.Logf("purge relay batch from %d status=%s", start, s)
		}
	}
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	st0 = statusOn(0, 5, 2*time.Minute)
	st1 = statusOn(1, 5, 2*time.Minute)
	c.rec("SHADOW-PURGED", "both emptied generations reach Purged after the grace window, which releases their committees' bond lock",
		st0 == 5 && st1 == 5, fmt.Sprintf("gen0 status=%s gen1 status=%s gen2 status=%s",
			statusStr(st0), statusStr(st1), statusStr(vfVaultStatusOn(d, ctx, 2, cid, 2))))

	finish()
	t.Logf("SHADOW COMPLETE CONTRACT=%s", cid)
}
