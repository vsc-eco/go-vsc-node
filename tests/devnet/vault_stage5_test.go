package devnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultStage5Lifecycle batches MORE cases on ONE devnet: money edges +
// the full retire→purge tail after a drain. v2-ON via Hpin-after-genesis.
//
//	VAULT_STAGE5_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultStage5Lifecycle -timeout 40m ./tests/devnet/
func TestVaultStage5Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_STAGE5_RUN") == "" {
		t.Skip("set VAULT_STAGE5_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := devnetWithTimeout(context.Background(), vfTestBudget(38*time.Minute))
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
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
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "vault stage5", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s", cid)
	vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH))
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	devnetSleep(10 * time.Second)

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

	// genesis gen-0 (v2-off)
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("gen0 keygen: %v", err)
	}
	primary0 := kd0.PublicKey
	// VR2-09: let the post-DKG pre-parameter regeneration finish before the check-sig.
	vfWaitPreparams(t, d, ctx, 12*time.Minute)
	if s := vfRegisterGenesis(t, d, ctx, 1, cid,
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary0, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen0 register: %s", s)
	}
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	rec("VL-GP-02", "genesis active", true, owner)

	// fund gen-0 with TWO deposits (multi-UTXO for a multi-input sweep later)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 60_000_000, seedH)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 40_000_000, contractLastHeight(t, d, ctx, cid))
	rec("MD03-GP-01/GP-09", "two deposits credited (multi-UTXO)", balanceCredited(t, d, ctx, cid, owner),
		fmt.Sprintf("%s=%d", owner, balanceSats(t, d, ctx, cid, owner)))

	// ── money edges (v2-off, gen-0 active) ──
	// PEN-04: zero-amount unmap must reject
	s0 := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"0","to":"%s"}`, mustNewBtcAddr(t, d, ctx)))
	rec("MD03-PEN-04", "zero-amount unmap rejected", !isOK(s0), "status="+s0)

	// GP-10: approve spender then unmapFrom (third-party withdrawal via allowance).
	// The allowance must authorize the FULL debit from the owner = amount + vscFee + btcFee
	// (contract handlers.go:198 checkAndDeductBalance(from, finalAmt=amount+fees)). Approving
	// EXACTLY the amount and unmapping the amount fee-on-top would exceed the allowance and be
	// (correctly) rejected — that was a test-setup boundary bug, not a contract bug. Use
	// deduct_fee=true so the fee comes out of the amount (finalAmt = amount = allowance): the
	// realistic "spend my exact allowance" flow. It also zeroes the allowance so PEN-06 rejects.
	spender := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 2)
	vstatus(t, d, ctx, 1, cid, "approve", fmt.Sprintf(`{"spender":"%s","amount":"%d"}`, spender, 5_000_000))
	devnetSleep(6 * time.Second)
	spendsBefore := txSpendIds(t, d, ctx, cid)
	sUF := vstatus(t, d, ctx, 2, cid, "unmapFrom",
		fmt.Sprintf(`{"amount":"%d","to":"%s","from":"%s","deduct_fee":true}`, 5_000_000, mustNewBtcAddr(t, d, ctx), owner))
	rec("MD03-GP-10", "unmapFrom via allowance accepted (exact allowance, deduct_fee)", isOK(sUF), "status="+sUF)

	// PEN-06: unmapFrom beyond allowance must reject
	sUF2 := vstatus(t, d, ctx, 2, cid, "unmapFrom",
		fmt.Sprintf(`{"amount":"%d","to":"%s","from":"%s"}`, 50_000_000, mustNewBtcAddr(t, d, ctx), owner))
	rec("MD03-PEN-06", "unmapFrom beyond allowance rejected", !isOK(sUF2), "status="+sUF2)

	// F1: activateKey refuses while the active generation still has an in-flight user
	// unmap (it could be stranded once the key retires), so GP-10's withdrawal is settled
	// before the rotation, the contract's settle-before-activate path.
	ufTxid := ""
	for i := 0; i < 20 && ufTxid == ""; i++ {
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(spendsBefore, id) {
				ufTxid = id
				break
			}
		}
		if ufTxid == "" {
			devnetSleep(3 * time.Second)
		}
	}
	sSettle := "NO_PENDING_SPEND"
	if ufTxid != "" {
		sSettle = settlePendingUnmap(t, d, ctx, cid, cid+"-main", ufTxid)
	}
	rec("F1-SETTLE", "GP-10's unmapFrom withdrawal is signed, broadcast and settled before the rotation", isOK(sSettle),
		fmt.Sprintf("txid=%s confirmSpend=%s", ufTxid, sSettle))

	// ── rotate to gen-1 under v2 ──
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
	activated := false
	for i := 0; i < 20; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
			activated = true
			break
		}
		devnetSleep(15 * time.Second)
	}
	rec("VL-GP-11/BRK-2", "gen-1 activated after check-sig (v2)", activated, "")
	if !activated {
		return
	}

	// PEN-04(NN#3): createKey while gen-0 still holds funds must REJECT
	sNN3 := vstatus(t, d, ctx, 1, cid, "createKey", "")
	rec("VL-PEN-04/NN#3", "createKey while superseded gen funded rejected", !isOK(sNN3), "status="+sNN3)

	// seed fee reserve + drain gen-0 → gen-1 (multi-input sweep across 2 UTXOs)
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)

	// ── retire tail: draining → inactive → (grace) → purged ──
	// migrate may need multiple tranches; loop migrate+settle until gen-0 empty, then retire.
	for i := 0; i < 3; i++ {
		if !isOK(vstatus(t, d, ctx, 1, cid, "migrateVault", "")) {
			break // nothing left to migrate
		}
		devnetSleep(4 * time.Second)
	}
	sRet1 := vstatus(t, d, ctx, 1, cid, "retireVault", "")
	rec("VL-GP-10a", "retireVault draining→inactive", isOK(sRet1), "status="+sRet1)
	// mine past the purge grace (VaultPurgeGraceBlocks=144 BTC blocks) + relay the headers in
	// BATCHES. addBlocks accepts many concatenated 80-byte headers (blocklist.go:85-91,128);
	// relaying one-by-one is ~150 confirmed contract calls = the 39-min timeout, so batch ~25
	// headers/call → ~7 calls.
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
	sRet2 := vstatus(t, d, ctx, 1, cid, "retireVault", "")
	rec("VL-GP-10b", "retireVault inactive→purged (after grace)", isOK(sRet2), "status="+sRet2)

	t.Logf("STAGE-5 SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}

// settlePendingUnmap signs, broadcasts, mines and confirms a user unmap the caller already
// created (unmapAndSettle does the same for an unmap it makes itself). It returns the
// confirmSpend status, or a short reason when the spend never reached confirmSpend.
func settlePendingUnmap(t *testing.T, d *Devnet, ctx context.Context, cid, keyId, txid string) string {
	t.Helper()
	sd := waitSigningData(t, d, ctx, cid, txid)
	if sd == nil {
		return "NO_SIGNING_DATA"
	}
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		return "DESERIALIZE_FAILED"
	}
	for _, uh := range sd.UnsignedSigHashes {
		sig := waitSignature(t, d, ctx, keyId, uh.SigHash)
		if sig == nil {
			return "NOT_SIGNED"
		}
		signature := append(append([]byte{}, sig...), byte(txscript.SigHashAll))
		mtx.TxIn[uh.Index].Witness = wire.TxWitness{signature, []byte{0x01}, uh.WitnessScript}
	}
	var buf bytes.Buffer
	if err := mtx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding); err != nil {
		return "ENCODE_FAILED"
	}
	bcTxid, err := d.bitcoinCli(ctx, "sendrawtransaction", hex.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Logf("unmap %s broadcast rejected: %v", txid, err)
		return "BROADCAST_REJECTED"
	}
	h, _ := d.MineBlocks(ctx, 1)
	// The settle waits MinConfirmationDepth, and addBlocks must chain onto the contract's
	// own last header, so bury the block and relay from the contract's height.
	tipAfter, _ := d.MineBlocks(ctx, vfDepositMaturityBlocks)
	for hh := contractLastHeight(t, d, ctx, cid) + 1; hh <= tipAfter; hh++ {
		hx, _ := btcBlockHeaderHex(ctx, d, hh)
		vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx))
	}
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", bhash, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	json.Unmarshal([]byte(blockJSON), &blk)
	if len(blk.Tx) < 2 {
		return "BLOCK_UNREADABLE"
	}
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", bcTxid)
	proof := reverseHexBytes(blk.Tx[0])
	return vstatus(t, d, ctx, 1, cid, "confirmSpend", fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"indices":[0]}`, h, rawTx, proof))
}

func mustNewBtcAddr(t *testing.T, d *Devnet, ctx context.Context) string {
	a, err := d.bitcoinCli(ctx, "getnewaddress")
	if err != nil {
		t.Fatalf("getnewaddress: %v", err)
	}
	return a
}
