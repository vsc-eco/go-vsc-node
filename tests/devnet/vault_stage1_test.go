package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultStage1StateMachine is the Stage-1 batched probe of the BTC
// vault-rotation-v2 GENERATION STATE MACHINE, with v2 ENABLED (Hpin low),
// driving createKey -> node keygen -> registerPublicKey -> activateKey and
// observing the transitions. It fakes NO btc money yet (Stage 2/3). It is
// deliberately log-heavy and soft-asserting so ONE devnet run reveals the exact
// behavior of every step (does createKey trigger keygen? what pubkey format does
// registerPublicKey want? does genesis need a backup? does BRK-2 check-sig gate
// activate?). Run:
//
//	L7_DEVNET_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultStage1StateMachine -timeout 30m ./tests/devnet/
func TestVaultStage1StateMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_STAGE1_RUN") == "" {
		t.Skip("set VAULT_STAGE1_RUN=1 to run the vault stage-1 probe")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm not found %s: %v", wasm, err)
	}

	cfg := tssTestConfig() // 5 nodes, ElectionInterval=20, generous TSS timeouts
	cfg.SkipFunding = false // contract DEPLOY needs the deployer funded with HBD
	cfg.EnableBitcoind = true
	// v2 ENABLED from a low height so the node runs S3 output-scoping + BRK-2 check-sig.
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = 1
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, ctxDev := startDevnetNoKey(t, cfg, 28*time.Minute)
	_ = ctxDev

	// ---- Bitcoin regtest seed ----
	if _, err := d.MineBlocks(ctx, 10); err != nil {
		t.Fatalf("mine btc: %v", err)
	}
	hdr, err := btcBlockHeaderHex(ctx, d, 1)
	if err != nil {
		t.Fatalf("btc header: %v", err)
	}
	t.Logf("btc seed header(1)=%s...", hdr[:24])

	// ---- Deploy btc-mapping-contract ----
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract",
		Description: "vault stage1", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s", cid)

	// seedBlocks so the contract has a chain tip
	seed := fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr, 1)
	logCall(t, d, ctx, 1, cid, "seedBlocks", seed)

	// Register BTC contract + restart so the node routes vault behavior (v2 scoping).
	if err := d.WriteOracleConfigs(ctx); err != nil {
		t.Logf("WriteOracleConfigs: %v", err)
	}
	if err := d.SetOracleContractIDs(map[string]string{"BTC": cid}); err != nil {
		t.Logf("SetOracleContractIDs: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Logf("restart: %v", err)
	}
	time.Sleep(10 * time.Second)

	// ---- createKey -> does it trigger node keygen? ----
	logCall(t, d, ctx, 1, cid, "createKey", "")
	keyId := cid + "-main"
	t.Logf("waiting for keygen of %s ...", keyId)
	kd, err := d.WaitForTssKey(ctx, 2, bson.M{"id": keyId, "status": "active"}, 8*time.Minute)
	if err != nil {
		// maybe the keyId suffix differs — dump all tss_keys to learn the real id
		all, _ := d.GetTssKeys(ctx, 2, bson.M{})
		t.Logf("WaitForTssKey failed: %v. All tss_keys on node2:", err)
		for _, k := range all {
			t.Logf("  tss_key id=%s status=%s algo=%s pub=%.20s", k.Id, k.Status, k.Algo, k.PublicKey)
		}
		t.Fatalf("createKey did NOT produce an active keygen for %s", keyId)
	}
	t.Logf("KEYGEN OK: id=%s status=%s algo=%s pubkey=%s", kd.Id, kd.Status, kd.Algo, kd.PublicKey)

	// ---- registerPublicKey (try the keygen pubkey as primary; probe backup handling) ----
	reg := fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, kd.PublicKey, kd.PublicKey)
	logCall(t, d, ctx, 1, cid, "registerPublicKey", reg)

	// ---- read back the stored primary key (did genesis write the flat key?) ----
	st, err := d.GetStateByKeys(ctx, 2, cid, []string{"pubkey", "backupkey", "v", "nextGen", "activeGen"})
	if err != nil {
		t.Logf("GetStateByKeys: %v", err)
	} else {
		for k, v := range st {
			t.Logf("STATE[%s] = %v", k, v)
		}
	}

	// ---- activateKey (BRK-2 check-sig gated under v2) ----
	logCall(t, d, ctx, 1, cid, "activateKey", "")
	time.Sleep(15 * time.Second)
	st2, _ := d.GetStateByKeys(ctx, 2, cid, []string{"pubkey", "backupkey", "v", "nextGen", "activeGen"})
	for k, v := range st2 {
		t.Logf("POST-ACTIVATE STATE[%s] = %v", k, v)
	}

	t.Logf("STAGE-1 PROBE COMPLETE — inspect the transitions above. CONTRACT=%s", cid)
}

// btcBlockHeaderHex reads block `height`'s 80-byte header hex from the devnet
// bitcoind (getblockhash + getblockheader false).
func btcBlockHeaderHex(ctx context.Context, d *Devnet, height uint64) (string, error) {
	hash, err := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(height))
	if err != nil {
		return "", fmt.Errorf("getblockhash: %w", err)
	}
	hdr, err := d.bitcoinCli(ctx, "getblockheader", hash, "false")
	if err != nil {
		return "", fmt.Errorf("getblockheader: %w", err)
	}
	return hdr, nil
}

// logCall calls a contract action and logs the (txid, err) without failing the
// test — so a probe run surfaces every step's outcome.
func logCall(t *testing.T, d *Devnet, ctx context.Context, node int, cid, action, payload string) {
	t.Helper()
	res, err := d.CallContract(ctx, node, cid, action, payload)
	if err != nil {
		t.Logf("CALL %s -> ERR: %v", action, err)
		return
	}
	t.Logf("CALL %s -> ok (%s)", action, res)
}
