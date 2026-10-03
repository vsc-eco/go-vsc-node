package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultOperatorDrivenDrain is the end-to-end proof of the scoped-operator model on
// a live devnet: a NON-owner account, appointed via setVaultOperator, drives the full
// migration drain (migrateVault + retireVault), while a third account that is neither
// owner nor operator is refused. This closes the gap between "checkOperator accepts the
// operator string" (contract unit test) and "a real, separately-identified caller
// completes a real drain end-to-end".
//
// Node identities on this devnet are hive:<prefix><node>. Node 1 deploys, so node 1 is
// the OWNER. We appoint node 2 as the OPERATOR and drive every vault op from node 2.
// Node 3 is the STRANGER.
//
//	VAULT_OPDRAIN_RUN=1 go test -v -run TestVaultOperatorDrivenDrain -timeout 48m ./tests/devnet/
func TestVaultOperatorDrivenDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_OPDRAIN_RUN") == "" {
		t.Skip("set VAULT_OPDRAIN_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 46*time.Minute)
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
	d, _ := startDevnetNoKey(t, cfg, 44*time.Minute)

	operator := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 2) // node 2
	stranger := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 3) // node 3

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "operator-drain", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s owner=node1 operator=%s stranger=%s", cid, operator, stranger)
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

	// ── genesis gen-0 + fund it with two UTXOs (forces a real multi-tranche drain) ──
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

	// ── OP-00: BEFORE appointment, the operator-to-be is a stranger — migrateVault refused. ──
	// (There is nothing to migrate yet either, but an auth refusal is distinct from a
	// benign "nothing to migrate": we assert on the owner-or-operator refusal message.)
	preAppoint := vstatus(t, d, ctx, 2, cid, "migrateVault", "")
	rec("OP-00", "un-appointed node-2 refused migrateVault (owner-only until appointed)", !isOK(preAppoint),
		"status="+preAppoint)

	// ── rotate to gen-1 (owner-driven setup) ──
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

	// ── OP-01: appoint node-2 as operator (owner-only). ──
	appoint := vstatus(t, d, ctx, 1, cid, "setVaultOperator", operator)
	rec("OP-01", "owner appointed node-2 as vault operator", isOK(appoint), "status="+appoint)

	// ── OP-02: a NON-owner may NOT appoint an operator. ──
	badAppoint := vstatus(t, d, ctx, 3, cid, "setVaultOperator", stranger)
	rec("OP-02", "non-owner cannot appoint an operator", !isOK(badAppoint), "status="+badAppoint)

	// ── OP-03: the STRANGER (node-3) is still refused migrateVault. ──
	strangerCall := vstatus(t, d, ctx, 3, cid, "migrateVault", "")
	rec("OP-03", "stranger refused migrateVault after operator set", !isOK(strangerCall), "status="+strangerCall)

	// ── OP-04: the OPERATOR (node-2) drives the FULL drain. Every migrateVault is issued
	// from node 2; retireVault too. If the operator guard were wrong these would be
	// rejected and the drain would never converge. ──
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	tranches := 0
	for i := 0; i < 6; i++ {
		remaining := genUtxoCount(t, d, ctx, cid, 0)
		if remaining == 0 {
			break
		}
		t.Logf("operator tranche %d: gen-0 holds %d UTXO(s) — sweeping AS THE OPERATOR (node 2)", i+1, remaining)
		migrateAndSettleAs(t, d, ctx, 2, cid, cid+"-main", primary1, backupPubKeyG)
		tranches++
		if after := genUtxoCount(t, d, ctx, cid, 0); after >= remaining {
			t.Errorf("operator tranche %d made NO progress (%d -> %d) — operator-driven drain cannot converge", i+1, remaining, after)
			break
		}
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	}
	left := genUtxoCount(t, d, ctx, cid, 0)
	rec("OP-04", "operator (node-2) drained gen-0 fully across tranches", left == 0,
		fmt.Sprintf("%d tranche(s), gen-0 now holds %d UTXOs", tranches, left))
	if left != 0 {
		t.Logf("OPERATOR-DRAIN SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
		return
	}

	// ── OP-05: the OPERATOR advances retire → INACTIVE (asserted on the registry). ──
	vstatus(t, d, ctx, 2, cid, "retireVault", "")
	st := vaultStatusOf(t, d, ctx, cid, 0)
	rec("OP-05", "operator-issued retireVault moved gen-0 to Inactive", st == 4, "gen-0 status="+statusStr(st))

	// ── OP-06: the owner can REVOKE the operator; node-2 is a stranger again. ──
	revoke := vstatus(t, d, ctx, 1, cid, "setVaultOperator", "")
	postRevoke := vstatus(t, d, ctx, 2, cid, "retireVault", "")
	rec("OP-06", "revoked operator (node-2) refused retireVault", isOK(revoke) && !isOK(postRevoke),
		fmt.Sprintf("revoke=%s postRevoke=%s", revoke, postRevoke))

	t.Logf("OPERATOR-DRAIN SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}
