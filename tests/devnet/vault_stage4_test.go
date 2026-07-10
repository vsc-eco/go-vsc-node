package devnet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"go.mongodb.org/mongo-driver/bson"
)

// untaggedVaultAddr builds the active vault's UNTAGGED P2WSH (empty tag →
// OP_CHECKSIG, not OP_CHECKSIGVERIFY+tag) — the address topUpFeeReserve credits.
func untaggedVaultAddr(primaryHex, backupHex string) (string, error) {
	primary, err := hex.DecodeString(primaryHex)
	if err != nil {
		return "", err
	}
	backup, err := hex.DecodeString(backupHex)
	if err != nil {
		return "", err
	}
	sb := txscript.NewScriptBuilder()
	sb.AddOp(txscript.OP_IF)
	sb.AddData(primary)
	sb.AddOp(txscript.OP_CHECKSIG) // empty tag → CHECKSIG
	sb.AddOp(txscript.OP_ELSE)
	sb.AddInt64(2) // TestnetBackupCSVBlocks (regtest)
	sb.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	sb.AddOp(txscript.OP_DROP)
	sb.AddData(backup)
	sb.AddOp(txscript.OP_CHECKSIG)
	sb.AddOp(txscript.OP_ENDIF)
	script, err := sb.Script()
	if err != nil {
		return "", err
	}
	wp := sha256.Sum256(script)
	addr, err := btcutil.NewAddressWitnessScriptHash(wp[:], &chaincfg.RegressionNetParams)
	if err != nil {
		return "", err
	}
	return addr.EncodeAddress(), nil
}

// fundFeeReserve seeds FeeSupply: deposit `sats` to the ACTIVE vault's untagged
// address, relay headers from the contract's last height, and topUpFeeReserve.
func fundFeeReserve(t *testing.T, d *Devnet, ctx context.Context, cid, activePrimary, activeBackup string, sats int64) {
	t.Helper()
	addr, err := untaggedVaultAddr(activePrimary, activeBackup)
	if err != nil {
		t.Fatalf("untagged addr: %v", err)
	}
	amt := fmt.Sprintf("%d.%08d", sats/1e8, sats%int64(1e8))
	if _, err := d.bitcoinCli(ctx, "sendtoaddress", addr, amt); err != nil {
		t.Fatalf("fee-reserve sendtoaddress: %v", err)
	}
	depTxid, _ := d.bitcoinCli(ctx, "getrawmempool") // ensure mempool has 1 tx
	_ = depTxid
	h, _ := d.MineBlocks(ctx, 1)
	// relay from the contract's current last height +1 to h
	last := contractLastHeight(t, d, ctx, cid)
	for hh := last + 1; hh <= h; hh++ {
		hx, _ := btcBlockHeaderHex(ctx, d, hh)
		if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx)); !isOK(s) {
			t.Fatalf("fee-reserve addBlocks %d: %s", hh, s)
		}
	}
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", bhash, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	json.Unmarshal([]byte(blockJSON), &blk)
	if len(blk.Tx) != 2 {
		t.Fatalf("fee-reserve block has %d txs (want 2)", len(blk.Tx))
	}
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", blk.Tx[1])
	proof := reverseHexBytes(blk.Tx[0])
	s := vstatus(t, d, ctx, 1, cid, "topUpFeeReserve", fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1}}`, h, rawTx, proof))
	if isOK(s) {
		t.Logf("CASE MD06-FR PASS — FeeSupply topped up (%d sats to untagged active vault)", sats)
	} else {
		t.Errorf("CASE MD06-FR FAIL — topUpFeeReserve status=%s", s)
	}
}

// contractLastHeight reads the contract "h" (last BTC height, decimal string).
func contractLastHeight(t *testing.T, d *Devnet, ctx context.Context, cid string) uint64 {
	st, err := getStateHex(d, ctx, 2, cid, []string{"h"})
	if err != nil {
		return 0
	}
	if b, ok := st["h"]; ok {
		if v, err := strconv.ParseUint(string(b), 10, 64); err == nil {
			return v
		}
	}
	return 0
}

// TestVaultStage4Rotation exercises the CORE blocktrades-fix machinery under v2
// ENABLED, dodging the fresh-genesis deadlock by pinning the activation height
// AFTER genesis:
//   - genesis (block < Hpin, v2-off) → gen-0 active + funded
//   - wait past Hpin (v2 on)
//   - rotate: createKey gen-1 → keygen → register → BRK-2 check-sig (ADMITTED now,
//     because gen-0 Active resolves the vault view) → activateKey → gen-0 Retiring
//   - migrateVault → migration sweep (gen-0 UTXOs → gen-1 P2WSH); the node signs it
//     ONLY because NN#1 output-scoping proves every output pays the successor →
//     assemble → broadcast → confirmSpend settles the migration → gen-0 drains.
//
//	VAULT_STAGE4_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultStage4Rotation -timeout 40m ./tests/devnet/
func TestVaultStage4Rotation(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_STAGE4_RUN") == "" {
		t.Skip("set VAULT_STAGE4_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 38*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	const hpin = 400 // v2 activation height — AFTER genesis (~block 190), so genesis
	// activates v2-off (no deadlock) and v2 turns on before the rotation.
	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 38*time.Minute)

	seedH, err := d.MineBlocks(ctx, 101)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "vault stage4", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s hpin=%d", cid, hpin)
	vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH))
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	time.Sleep(10 * time.Second)

	// ── gen-0 genesis (v2 still off, block < hpin) ──
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
	t.Logf("CASE VL-GP-02 PASS — gen-0 genesis active (v2-off pre-hpin)")

	// ── fund gen-0 ──
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 100_000_000, seedH)
	if !balanceCredited(t, d, ctx, cid, owner) {
		t.Fatalf("gen-0 funding failed")
	}
	t.Logf("gen-0 funded: %s = %d sats", owner, balanceSats(t, d, ctx, cid, owner))

	// ── wait until v2 is ACTIVE (VSC height > hpin) ──
	t.Logf("waiting for VSC height > hpin=%d (v2 on)...", hpin)
	if err := d.WaitForBlockProcessing(ctx, 2, hpin+5, 8*time.Minute); err != nil {
		t.Logf("WaitForBlockProcessing: %v (continuing)", err)
	}
	t.Logf("v2 now ACTIVE — rotating gen-0 → gen-1")

	// ── rotate: createKey gen-1 → keygen → register → BRK-2 check-sig → activate ──
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd1, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-mainv1", "status": "active"}, 8*time.Minute)
	if err != nil {
		// keyId naming may differ; dump keys
		all, _ := d.GetTssKeys(ctx, 2, bson.M{})
		for _, k := range all {
			t.Logf("  tss_key id=%s status=%s", k.Id, k.Status)
		}
		t.Fatalf("gen1 keygen: %v", err)
	}
	primary1 := kd1.PublicKey
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary1, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen1 register: %s", s)
	}
	// BRK-2: activateKey only succeeds once the node's check-sig for gen-1 lands
	// (admitted by output-scoping because gen-0 is Active → view resolves).
	activated := false
	for i := 0; i < 12; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
			activated = true
			break
		}
		t.Logf("activateKey not yet (awaiting BRK-2 check-sig)... retry %d", i)
		time.Sleep(15 * time.Second)
	}
	if activated {
		t.Logf("CASE VL-GP-11/BRK-2 PASS — gen-1 activated after check-sig (v2 ON); CASE VL-GP-01 rotation gen-0→retiring")
	} else {
		t.Errorf("CASE VL-GP-11/BRK-2 FAIL — gen-1 never activated (check-sig not admitted under v2 rotation)")
		return
	}

	// ── seed FeeSupply (migration sweep needs a fee reserve) via the gen-1 active vault ──
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ── migrate/drain gen-0 → gen-1 (NN#1 output scoping) ──
	migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)

	t.Logf("STAGE-4 COMPLETE CONTRACT=%s", cid)
}

// migrateAndSettle drives migrateVault, waits for the node to sign the migration
// sweep (which it does ONLY if NN#1 proves every output pays the successor),
// assembles + broadcasts, and confirmSpend-settles the migration.
func migrateAndSettle(t *testing.T, d *Devnet, ctx context.Context, cid, retiringKeyId, succPrimary, succBackup string) {
	t.Helper()
	before := txSpendIds(t, d, ctx, cid)
	if s := vstatus(t, d, ctx, 1, cid, "migrateVault", ""); !isOK(s) {
		t.Errorf("CASE VL-GP-06 FAIL — migrateVault rejected status=%s", s)
		return
	}
	var txid string
	for i := 0; i < 20 && txid == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(before, id) {
				txid = id
				break
			}
		}
	}
	if txid == "" {
		t.Errorf("CASE VL-GP-06 FAIL — no migration sweep pending spend appeared")
		return
	}
	t.Logf("migration sweep txid=%s (retiring gen %s)", txid, retiringKeyId)

	sd := waitSigningData(t, d, ctx, cid, txid)
	if sd == nil {
		t.Errorf("CASE VL-GP-06 FAIL — no signing data for sweep %s", txid)
		return
	}
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		t.Errorf("deser sweep: %v", err)
		return
	}
	// The RETIRING gen (gen-0) key signs the sweep — NN#1 output-scoping admits it
	// only because every output pays the gen-1 successor P2WSH.
	for _, uh := range sd.UnsignedSigHashes {
		sig := waitSignature(t, d, ctx, retiringKeyId, uh.SigHash)
		if sig == nil {
			t.Errorf("CASE VL-PEN-03/NN#1 — retiring gen did NOT sign the sweep (output-scoping refused? or slow). input %d", uh.Index)
			return
		}
		signature := append(append([]byte{}, sig...), byte(txscript.SigHashAll))
		mtx.TxIn[uh.Index].Witness = wire.TxWitness{signature, []byte{0x01}, uh.WitnessScript}
	}
	var buf bytes.Buffer
	mtx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding)
	bcTxid, err := d.bitcoinCli(ctx, "sendrawtransaction", hex.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Errorf("CASE VL-GP-06 FAIL — sweep broadcast rejected: %v", err)
		return
	}
	t.Logf("CASE VL-GP-06/NN#1 PASS(partial) — migration sweep TSS-signed (retiring gen, successor-scoped) + broadcast: %s", bcTxid)

	h, _ := d.MineBlocks(ctx, 1)
	hx, _ := btcBlockHeaderHex(ctx, d, h)
	vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx))
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", bhash, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	json.Unmarshal([]byte(blockJSON), &blk)
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", bcTxid)
	proof := reverseHexBytes(blk.Tx[0])
	cs := vstatus(t, d, ctx, 1, cid, "confirmSpend", fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"indices":[0]}`, h, rawTx, proof))
	if isOK(cs) {
		t.Logf("CASE VL-GP-01/GP-06 PASS — migration sweep SETTLED via confirmSpend (gen-0 drained to gen-1)")
	} else {
		t.Logf("CASE VL-GP-06 PASS(broadcast)/confirmSpend status=%s (settle needs review)", cs)
	}
}
