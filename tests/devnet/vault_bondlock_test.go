package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// pendingConsensusUnstake sums an account's pending consensus_unstake amount from a
// node's ledger_actions (the same predicate the ledger's
// GetAccountPendingConsensusUnstake uses). A refused unstake creates NO such action;
// an accepted one creates a pending action — so this cleanly distinguishes the two.
func pendingConsensusUnstake(t *testing.T, d *Devnet, ctx context.Context, node int, account string) int64 {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	cur, err := client.Database(d.nodeDbName(node)).Collection("ledger_actions").
		Find(ctx, bson.M{"to": account, "status": "pending", "type": "consensus_unstake"})
	if err != nil {
		t.Fatalf("ledger_actions query: %v", err)
	}
	var rows []struct {
		Amount int64 `bson:"amount"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		t.Fatalf("decode ledger_actions: %v", err)
	}
	var total int64
	for _, r := range rows {
		total += r.Amount
	}
	return total
}

// TestVaultBondLock proves the #11 bond-lock consensus-unstake gate end-to-end: a
// committee member whose generation holds a retiring, not-yet-drained BTC vault key
// CANNOT unstake its consensus bond, and CAN once that generation is drained. The
// same unstake op is refused while gen-0 is retiring+funded and accepted after gen-0
// drains — so the difference isolates the bond-lock and its release (an insufficient-
// stake refusal would fail BOTH; a bond-lock fails only the first).
//
//	VAULT_BONDLOCK_RUN=1 go test -v -run TestVaultBondLock -timeout 45m ./tests/devnet/
func TestVaultBondLock(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_BONDLOCK_RUN") == "" {
		t.Skip("set VAULT_BONDLOCK_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 43*time.Minute)
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
	d, _ := startDevnetNoKey(t, cfg, 41*time.Minute)

	// The unstaking member: a witness that participated in gen-0's keygen (all devnet
	// nodes do), so it is in the retiring signer set once gen-0 is superseded.
	const unstakeNode = 3
	member := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, unstakeNode)

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "bond-lock", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s bond-lock member=%s", cid, member)
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

	// ── genesis gen-0 + one funded UTXO, then rotate so gen-0 is retiring ──
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
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 50_000_000, seedH)

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
	for i := 0; i < 20; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
			break
		}
		time.Sleep(15 * time.Second)
	}
	// gen-0 is now retiring and still funded → member's bond is locked.

	// ── BOND-01: the unstake is REFUSED while gen-0 is retiring+funded. ──
	// Terminal-status read (H-25): FAILED + pending 0 is a refusal.
	lockedStatus, lockedPending := vfUnstakeVerdict(t, d, ctx, unstakeNode, 3*time.Minute)
	rec("BOND-01", "consensus_unstake REFUSED while the member's gen is retiring+funded (bond-locked)",
		lockedStatus == "FAILED" && lockedPending == 0, fmt.Sprintf("pending consensus_unstake=%d (want 0)", lockedPending))

	// ── drain gen-0 fully → the lock releases ──
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	for i := 0; i < 6; i++ {
		if genUtxoCount(t, d, ctx, cid, 0) == 0 {
			break
		}
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	}
	drained := genUtxoCount(t, d, ctx, cid, 0)
	rec("BOND-01b", "gen-0 drained (precondition for the lock release)", drained == 0,
		fmt.Sprintf("gen-0 holds %d UTXO(s)", drained))
	// Also advance retire so gen-0 leaves the fund-holding set cleanly.
	vstatus(t, d, ctx, 1, cid, "retireVault", "")

	// ── BOND-02: the SAME unstake is now ACCEPTED (bond released). ──
	releasedStatus, releasedPending := vfUnstakeVerdict(t, d, ctx, unstakeNode, 3*time.Minute)
	rec("BOND-02", "consensus_unstake ACCEPTED once the gen is drained (bond released)",
		releasedStatus == "CONFIRMED" && releasedPending > 0, fmt.Sprintf("unstake status=%s (want CONFIRMED) pending consensus_unstake=%d (want >0)", releasedStatus, releasedPending))

	t.Logf("BONDLOCK SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}
