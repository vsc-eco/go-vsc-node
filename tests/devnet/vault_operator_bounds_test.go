package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultOperatorBounds is the adversarial proof that the scoped operator's power
// is BOUNDED: even a fully-compromised operator key cannot destroy funds or seize
// governance. It appoints an operator, then confirms that operator CANNOT:
//   - writeOffDust a generation holding a large, still-sweepable UTXO (no fund destruction)
//   - retireVault a still-funded generation (no premature purge / orphaning)
//   - setVaultOperator (no privilege escalation / self-re-appointment)
//   - pause the contract (no governance seizure)
//
// The safe operations (migrate to the committed successor, etc.) are proven by
// TestVaultOperatorDrivenDrain; this test proves the flip side — the operator's blast
// radius is exactly the four self-validating ops and nothing more.
//
//	VAULT_OPBOUNDS_RUN=1 go test -v -run TestVaultOperatorBounds -timeout 40m ./tests/devnet/
func TestVaultOperatorBounds(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_OPBOUNDS_RUN") == "" {
		t.Skip("set VAULT_OPBOUNDS_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 38*time.Minute)
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
	d, _ := startDevnetNoKey(t, cfg, 36*time.Minute)

	operator := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 2)

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "operator-bounds", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s operator=%s", cid, operator)
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

	// ── genesis gen-0 + a LARGE (very sweepable) deposit ──
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
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 80_000_000, seedH)

	// ── rotate to gen-1 so gen-0 is retiring + still holds its 80M UTXO ──
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

	// appoint node 2 as operator
	if s := vstatus(t, d, ctx, 1, cid, "setVaultOperator", operator); !isOK(s) {
		t.Fatalf("appoint operator: %s", s)
	}
	utxosBefore := genUtxoCount(t, d, ctx, cid, 0)
	if utxosBefore < 1 {
		t.Fatalf("expected gen-0 to still hold its large UTXO, got %d", utxosBefore)
	}

	// ── ADV-1: operator writeOffDust must NOT destroy a large, sweepable UTXO. ──
	// writeOffDust is gated on provable un-sweepability; 80M is nowhere near dust, so the
	// op runs but must delete NOTHING. A false positive here would let a compromised
	// operator burn real user funds.
	vstatus(t, d, ctx, 2, cid, "writeOffDust", "")
	afterWod := genUtxoCount(t, d, ctx, cid, 0)
	rec("ADV-1", "operator writeOffDust did NOT destroy the large sweepable UTXO", afterWod == utxosBefore,
		fmt.Sprintf("gen-0 held %d UTXO(s) before, %d after", utxosBefore, afterWod))

	// ── ADV-2: operator retireVault must NOT purge a still-funded generation. ──
	vstatus(t, d, ctx, 2, cid, "retireVault", "")
	st := vaultStatusOf(t, d, ctx, cid, 0)
	rec("ADV-2", "operator retireVault did NOT advance a still-funded gen (stays Retiring/Draining)", st == 2 || st == 3,
		"gen-0 status="+statusStr(st))

	// ── ADV-3: operator cannot ESCALATE — setVaultOperator is owner-only. ──
	escalate := vstatus(t, d, ctx, 2, cid, "setVaultOperator", operator)
	rec("ADV-3", "operator cannot appoint/escalate via setVaultOperator (owner-only)", !isOK(escalate),
		"status="+escalate)

	// ── ADV-4: operator cannot SEIZE GOVERNANCE — pause is owner-only. ──
	pauseAttempt := vstatus(t, d, ctx, 2, cid, "pause", "")
	rec("ADV-4", "operator cannot pause the contract (governance stays owner-only)", !isOK(pauseAttempt),
		"status="+pauseAttempt)

	// ── ADV-5: the gen-0 funds are still intact and still gen-0 after all the attacks. ──
	finalCount := genUtxoCount(t, d, ctx, cid, 0)
	rec("ADV-5", "gen-0 funds intact after every operator attack", finalCount == utxosBefore,
		fmt.Sprintf("gen-0 holds %d UTXO(s)", finalCount))

	t.Logf("OPERATOR-BOUNDS SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}
