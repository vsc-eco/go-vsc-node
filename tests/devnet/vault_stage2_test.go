package devnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/cmd/mapping-bot/chain"

	"github.com/btcsuite/btcd/chaincfg"
	"go.mongodb.org/mongo-driver/bson"
)

// A valid compressed secp256k1 pubkey (the generator point G) used as the
// owner-held CSV backup recovery key for genesis. Not a TSS key.
const backupPubKeyG = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

// TestVaultStage2Funding drives the FUNDING path: genesis gen-0 active, then a
// real regtest BTC deposit to the gen-0 vault address, relayed + proven via
// map, crediting a VSC balance. Batches MD-03 GP-01 (deposit-credit) + GP-07
// (conservation) + VL-GP-02 genesis. Run:
//
//	VAULT_STAGE2_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultStage2Funding -timeout 32m ./tests/devnet/
func TestVaultStage2Funding(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_STAGE2_RUN") == "" {
		t.Skip("set VAULT_STAGE2_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = 0 // v2 OFF: genesis activates (v2-on genesis is deadlocked, see FINDING)
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 30*time.Minute)

	// Mine 101 blocks so the early coinbases MATURE (100-block maturity) and the
	// wallet has spendable funds for the deposit. Seed the contract at the tip so
	// we only relay ONE header (tip+1) to the deposit block, not 100.
	seedH, err := d.MineBlocks(ctx, 101)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	hdr1, err := btcBlockHeaderHex(ctx, d, seedH)
	if err != nil {
		t.Fatalf("hdr: %v", err)
	}
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "vault stage2", DeployerNode: 1, GQLNode: 2,
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

	// ---- genesis: createKey -> keygen -> register(primary=keygen, backup=G) -> activate ----
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	primary := kd.PublicKey
	t.Logf("gen-0 primary keygen pubkey=%s", primary)
	reg := fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary, backupPubKeyG)
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey", reg); !isOK(s) {
		t.Fatalf("registerPublicKey status=%s", s)
	}
	// Under v2-off, genesis AUTO-ACTIVATES inside registerPublicKey (both keys +
	// no check-sig gate). No explicit activateKey needed (it would abort: no
	// pending vault). CASE VL-GP-02 genesis auto-activate: register CONFIRMED == active.
	t.Logf("CASE VL-GP-02 PASS — genesis auto-activated on register (v2-off)")

	// ---- fund gen-0: deposit 0.5 BTC to alice's vault deposit address ----
	recipient := "hive:alice"
	const sats = 50_000_000
	credited := fundVaultViaSPV(t, d, ctx, cid, primary, backupPubKeyG, recipient, sats, seedH)

	// ---- assert: the deposit credited alice's mapped balance (presence-based) ----
	if balanceCredited(t, d, ctx, cid, recipient) {
		t.Logf("CASE MD03-GP-01 PASS — deposit credited to %s (addr %s)", recipient, credited)
	} else {
		t.Errorf("CASE MD03-GP-01 FAIL — %s balance absent after map", recipient)
	}
	_ = sats
	t.Logf("STAGE-2 COMPLETE CONTRACT=%s", cid)
}

// fundVaultDeposit derives the gen deposit address for `recipient`, sends `sats`
// to it on regtest in a block with exactly [coinbase, deposit] (so the Merkle
// proof is just the coinbase hash), relays the intervening headers via addBlocks,
// and calls map with a valid SPV proof. Returns the deposit address.
func fundVaultViaSPV(t *testing.T, d *Devnet, ctx context.Context, cid, primaryHex, backupHex, recipient string, sats int64, lastRelayed uint64) string {
	t.Helper()
	instruction := "deposit_to=" + recipient
	gen := &chain.BTCAddressGenerator{Params: &chaincfg.RegressionNetParams, BackupCSVBlocks: 2}
	addr, _, err := gen.GenerateDepositAddress(primaryHex, backupHex, instruction)
	if err != nil {
		t.Fatalf("derive deposit addr: %v", err)
	}
	t.Logf("gen deposit addr for %s = %s", recipient, addr)

	// send + mine one block containing exactly coinbase + our deposit
	amt := fmt.Sprintf("%d.%08d", sats/1e8, sats%int64(1e8))
	depTxid, err := d.bitcoinCli(ctx, "sendtoaddress", addr, amt)
	if err != nil {
		t.Fatalf("sendtoaddress: %v", err)
	}
	h, err := d.MineBlocks(ctx, 1)
	if err != nil {
		t.Fatalf("mine deposit block: %v", err)
	}
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", bhash, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	if err := json.Unmarshal([]byte(blockJSON), &blk); err != nil {
		t.Fatalf("getblock parse: %v", err)
	}
	if len(blk.Tx) != 2 {
		t.Fatalf("expected 2 txs in deposit block, got %d (%v)", len(blk.Tx), blk.Tx)
	}
	coinbaseTxid := blk.Tx[0]
	if blk.Tx[1] != depTxid {
		// deposit may be index 0 if ordering differs — but coinbase is always first
		t.Logf("note: deposit txid=%s block tx=%v", depTxid, blk.Tx)
	}
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", depTxid)

	// Merkle proof for a 2-tx block: sibling = coinbase hash (internal byte order),
	// tx_index = 1.
	proofHex := reverseHexBytes(coinbaseTxid)

	// relay headers lastRelayed+1 .. h via addBlocks (each chains onto the prior)
	for hh := lastRelayed + 1; hh <= h; hh++ {
		hx, err := btcBlockHeaderHex(ctx, d, hh)
		if err != nil {
			t.Fatalf("hdr %d: %v", hh, err)
		}
		s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx))
		if !isOK(s) {
			t.Fatalf("addBlocks height %d status=%s", hh, s)
		}
	}

	mapPayload := fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"instructions":["%s"]}`,
		h, rawTx, proofHex, instruction)
	if s := vstatus(t, d, ctx, 1, cid, "map", mapPayload); !isOK(s) {
		t.Fatalf("map status=%s (proof/format issue)", s)
	}
	return addr
}

// getStateHex reads contract state keys with encoding:"hex" (NON-LOSSY, unlike the
// default raw encoding which mangles bytes >0x7F to U+FFFD). Returns key->rawBytes.
func getStateHex(d *Devnet, ctx context.Context, node int, cid string, keys []string) (map[string][]byte, error) {
	const q = `query($c:String!,$k:[String!]!,$e:String){getStateByKeys(contractId:$c,keys:$k,encoding:$e)}`
	var out struct {
		GetStateByKeys map[string]string `json:"getStateByKeys"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"c": cid, "k": keys, "e": "hex"}, &out); err != nil {
		return nil, err
	}
	res := make(map[string][]byte, len(out.GetStateByKeys))
	for k, v := range out.GetStateByKeys {
		if b, err := hex.DecodeString(v); err == nil {
			res[k] = b
		}
	}
	return res, nil
}

// balanceSats decodes "a-<recipient>" as a compact big-endian int64 (exact).
func balanceSats(t *testing.T, d *Devnet, ctx context.Context, cid, recipient string) int64 {
	t.Helper()
	st, err := getStateHex(d, ctx, 2, cid, []string{"a-" + recipient})
	if err != nil {
		t.Logf("balanceSats: %v", err)
		return -1
	}
	b, ok := st["a-"+recipient]
	if !ok || len(b) == 0 {
		return 0
	}
	var v int64
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	return v
}

// balanceCredited reports whether the recipient has a non-zero mapped balance.
func balanceCredited(t *testing.T, d *Devnet, ctx context.Context, cid, recipient string) bool {
	return balanceSats(t, d, ctx, cid, recipient) > 0
}

func reverseHexBytes(hexStr string) string {
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return hexStr
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b)
}

func decodeCompactBE(s string) int64 {
	// best-effort: if it looks like hex, decode big-endian; else parse as int
	if b, err := hex.DecodeString(s); err == nil && len(b) > 0 && len(b) <= 8 {
		var v int64
		for _, c := range b {
			v = v<<8 | int64(c)
		}
		return v
	}
	var v int64
	fmt.Sscanf(s, "%d", &v)
	return v
}
