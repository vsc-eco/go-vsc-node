package devnet

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultDrainToPurge proves the leg the PR's own suite never proves: that a
// retiring generation can be drained COMPLETELY and then actually retire.
//
// Two gaps in the existing tests motivate this:
//
//  1. getMigrationInputs sweeps only a bounded TRANCHE ("the caller drains it in
//     successive tranches"). Stage-5 loops migrateVault but never signs/broadcasts/
//     settles the extra tranches, so a multi-UTXO gen keeps most of its BTC.
//  2. Stage-5's retire cases assert only that the retireVault TRANSACTION confirmed
//     — and retireVault is a no-op-tolerant sweeper that succeeds while transitioning
//     NOTHING. They pass with gen-0 still sitting in Draining, still funded.
//
// So here every tranche is settled end-to-end, and every assertion reads the
// on-chain VAULT REGISTRY ("v") and UTXO registry ("r") — never a tx status.
//
//	VAULT_DRAIN_RUN=1 go test -v -run TestVaultDrainToPurge -timeout 45m ./tests/devnet/
func TestVaultDrainToPurge(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_DRAIN_RUN") == "" {
		t.Skip("set VAULT_DRAIN_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 43*time.Minute)
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
	d, _ := startDevnetNoKey(t, cfg, 41*time.Minute)

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "drain-to-purge", DeployerNode: 1, GQLNode: 2,
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

	// ── genesis gen-0 + fund it with MULTIPLE UTXOs (forces >1 tranche) ──
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
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 60_000_000, seedH)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 40_000_000, contractLastHeight(t, d, ctx, cid))
	n0 := genUtxoCount(t, d, ctx, cid, 0)
	rec("DRAIN-00", "gen-0 funded with multiple UTXOs", n0 >= 2, fmt.Sprintf("gen-0 holds %d UTXOs", n0))
	if n0 < 2 {
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
	// activateKey confirms slightly before the gen-0 → Retiring transition is committed
	// to the vault registry, so poll rather than read once (a single read races the flip
	// and gives a false "still Active").
	gen0Status := -1
	for i := 0; i < 8; i++ {
		gen0Status = vaultStatusOf(t, d, ctx, cid, 0)
		if gen0Status >= 2 { // Retiring/Draining/...
			break
		}
		time.Sleep(10 * time.Second)
	}
	rec("DRAIN-01", "gen-1 active, gen-0 retiring", gen0Status >= 2, "gen-0 status="+statusStr(gen0Status))

	// ── DRAIN LOOP: settle EVERY tranche until gen-0 holds nothing ──
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	tranches := 0
	for i := 0; i < 6; i++ {
		remaining := genUtxoCount(t, d, ctx, cid, 0)
		if remaining == 0 {
			break
		}
		t.Logf("tranche %d: gen-0 still holds %d UTXO(s) — sweeping", i+1, remaining)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		tranches++
		if after := genUtxoCount(t, d, ctx, cid, 0); after >= remaining {
			t.Errorf("tranche %d made NO progress (%d -> %d UTXOs) — drain cannot converge", i+1, remaining, after)
			break
		}
		// refill the fee reserve for the next tranche
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	}
	left := genUtxoCount(t, d, ctx, cid, 0)
	rec("DRAIN-02", "gen-0 fully drained across successive tranches", left == 0,
		fmt.Sprintf("%d tranche(s), gen-0 now holds %d UTXOs", tranches, left))
	if left != 0 {
		return
	}

	// ── retire: draining → INACTIVE. Asserted on the REGISTRY, not the tx status. ──
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	st := vaultStatusOf(t, d, ctx, cid, 0)
	rec("DRAIN-03", "gen-0 REALLY transitioned to Inactive (vault registry)", st == 4, "gen-0 status="+statusStr(st))

	// ── purge after the grace window: inactive → PURGED ──
	last := contractLastHeight(t, d, ctx, cid)
	h, _ := d.MineBlocks(ctx, 150)
	const relayBatch = 25
	for start := last + 1; start <= h; start += relayBatch {
		var hexBatch string
		for hh := start; hh < start+relayBatch && hh <= h; hh++ {
			hx, _ := btcBlockHeaderHex(ctx, d, hh)
			hexBatch += hx
		}
		vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hexBatch))
	}
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	st = vaultStatusOf(t, d, ctx, cid, 0)
	rec("DRAIN-04", "gen-0 REALLY transitioned to Purged (vault registry)", st == 5, "gen-0 status="+statusStr(st))

	t.Logf("DRAIN SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}

func statusStr(s int) string {
	names := []string{"Pending", "Active", "Retiring", "Draining", "Inactive", "Purged"}
	if s < 0 || s >= len(names) {
		return "unknown(" + strconv.Itoa(s) + ")"
	}
	return names[s]
}

// vaultStatusOf reads the committed vault registry ("v") and returns the status
// byte of the given generation, or -1 if absent. This is chain truth — the thing
// a tx-status assertion cannot see.
func vaultStatusOf(t *testing.T, d *Devnet, ctx context.Context, cid string, gen uint32) int {
	t.Helper()
	st, err := getStateHex(d, ctx, 2, cid, []string{"v"})
	if err != nil {
		t.Logf("vaultStatusOf: %v", err)
		return -1
	}
	vs, err := btcvault.UnmarshalVaultRegistry(st["v"])
	if err != nil {
		t.Logf("vaultStatusOf decode: %v", err)
		return -1
	}
	for _, v := range vs {
		if v.Generation == gen {
			return int(v.Status)
		}
	}
	return -1
}

// genUtxoCount counts the UTXOs in the committed registry ("r") that belong to
// the given generation. Zero == that generation is drained (what the contract's
// own fund-gate keys off).
func genUtxoCount(t *testing.T, d *Devnet, ctx context.Context, cid string, gen uint32) int {
	t.Helper()
	st, err := getStateHex(d, ctx, 2, cid, []string{"r"})
	if err != nil {
		t.Logf("genUtxoCount: %v", err)
		return -1
	}
	reg := st["r"]
	n := 0
	for off := 0; off+8 <= len(reg); off += 8 {
		id := uint16(reg[off])<<8 | uint16(reg[off+1])
		key := "u-" + strconv.FormatUint(uint64(id), 16)
		us, err := getStateHex(d, ctx, 2, cid, []string{key})
		if err != nil {
			continue
		}
		raw := us[key]
		if len(raw) < 4 {
			continue
		}
		// generation is the trailing uint32 (big-endian) of the utxo record
		g := uint32(raw[len(raw)-4])<<24 | uint32(raw[len(raw)-3])<<16 |
			uint32(raw[len(raw)-2])<<8 | uint32(raw[len(raw)-1])
		if g == gen {
			n++
		}
	}
	return n
}

var _ = hex.EncodeToString
