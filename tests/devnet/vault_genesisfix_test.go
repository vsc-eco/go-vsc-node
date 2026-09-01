package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultGenesisV2Fix DEVNET-proves the fix for the v2 fresh-genesis deadlock:
// with VaultRotationV2ActivationHeight=1 (v2 ON from the start), genesis must now
// ACTIVATE — the output-scoping admits the genesis gen's check-sig, so
// registerPublicKey/activateKey succeed (before the fix they FAILED forever).
//
//	VAULT_GENESISFIX_RUN=1 go test -v -run TestVaultGenesisV2Fix -timeout 30m ./tests/devnet/
func TestVaultGenesisV2Fix(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_GENESISFIX_RUN") == "" {
		t.Skip("set VAULT_GENESISFIX_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = 1 // v2 ON at genesis (the deadlock case)
	d, _ := startDevnetNoKey(t, cfg, 28*time.Minute)

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "genesis-fix", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s (v2 ON at genesis)", cid)
	vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH))
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	time.Sleep(10 * time.Second)

	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	primary := kd.PublicKey
	// register + activate, RETRYING until the (now-admitted) genesis check-sig lands.
	reg := fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary, backupPubKeyG)
	ok := false
	for i := 0; i < 14; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "registerPublicKey", reg)) {
			ok = true
			break
		}
		t.Logf("register not yet activated (awaiting genesis check-sig)... retry %d", i)
		time.Sleep(15 * time.Second)
	}
	if !ok {
		// try explicit activateKey path too
		for i := 0; i < 6; i++ {
			if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
				ok = true
				break
			}
			time.Sleep(15 * time.Second)
		}
	}
	if ok {
		t.Logf("CASE FINDING-FIX PASS — v2 fresh-genesis ACTIVATED (deadlock fixed); genesis check-sig admitted by output-scoping")
	} else {
		t.Errorf("CASE FINDING-FIX FAIL — v2 genesis still not activating (fix ineffective or check-sig still refused)")
	}
	t.Logf("GENESISFIX COMPLETE CONTRACT=%s", cid)
}
