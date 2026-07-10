package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultStage6PauseTheft batches contract-guard cases (v2-off, gen-0 active):
// pause matrix (MD07 G-01), non-owner pause reject (P-05), map-replay double-credit
// reject (MD03-PEN-01), topUpFeeReserve-of-a-non-deposit reject (MD06 P-FR).
//
//	VAULT_STAGE6_RUN=1 go test -v -run TestVaultStage6PauseTheft -timeout 30m ./tests/devnet/
func TestVaultStage6PauseTheft(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_STAGE6_RUN") == "" {
		t.Skip("set VAULT_STAGE6_RUN=1")
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
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = 0 // v2 off
	d, _ := startDevnetNoKey(t, cfg, 28*time.Minute)

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "stage6", DeployerNode: 1, GQLNode: 2,
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

	// genesis + fund
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	primary := kd.PublicKey
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary, backupPubKeyG)); !isOK(s) {
		t.Fatalf("register: %s", s)
	}
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	// capture the funding proof so we can replay it (PEN-01)
	fundH, rawTx, proof, instr := fundVaultCapture(t, d, ctx, cid, primary, backupPubKeyG, owner, 50_000_000, seedH)
	rec("MD03-GP-01", "deposit credited", balanceCredited(t, d, ctx, cid, owner), owner)

	// ── MD03-PEN-01: replay the SAME map tx → must NOT double-credit ──
	replay := fmt.Sprintf(`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"instructions":["%s"]}`, fundH, rawTx, proof, instr)
	sRe := vstatus(t, d, ctx, 1, cid, "map", replay)
	// either the map rejects, or it's a no-op (already-observed outpoint) — assert balance UNCHANGED
	after := balanceSats(t, d, ctx, cid, owner)
	rec("MD03-PEN-01", "map replay does not double-credit", after == 50_000_000, fmt.Sprintf("bal=%d status=%s", after, sRe))

	// ── MD07 G-01: pause halts token ops ──
	if s := vstatus(t, d, ctx, 1, cid, "pause", ""); !isOK(s) {
		t.Fatalf("pause: %s", s)
	}
	sMapP := vstatus(t, d, ctx, 1, cid, "map", replay)
	rec("MD07-G01a", "map rejected while paused", !isOK(sMapP), "status="+sMapP)
	sXferP := vstatus(t, d, ctx, 1, cid, "transfer", `{"amount":"1000000","to":"hive:bob"}`)
	rec("MD07-G01b", "transfer rejected while paused", !isOK(sXferP), "status="+sXferP)
	sUnmapP := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"1000000","to":"%s"}`, mustNewBtcAddr(t, d, ctx)))
	rec("MD07-G01c", "unmap rejected while paused", !isOK(sUnmapP), "status="+sUnmapP)

	// ── MD07 P-05: non-owner unpause rejected (node 2 ≠ owner) ──
	sNoOwn := vstatus(t, d, ctx, 2, cid, "unpause", "")
	rec("MD07-P05", "non-owner unpause rejected", !isOK(sNoOwn), "status="+sNoOwn)

	// ── unpause resumes ──
	if s := vstatus(t, d, ctx, 1, cid, "unpause", ""); !isOK(s) {
		t.Fatalf("unpause: %s", s)
	}
	sXferR := vstatus(t, d, ctx, 1, cid, "transfer", `{"amount":"1000000","to":"hive:bob"}`)
	rec("MD07-G01d", "transfer resumes after unpause", isOK(sXferR), "status="+sXferR)

	// ── MD06 P-FR: topUpFeeReserve pointing at a NON-deposit (the replay map tx) → reject ──
	sBadFR := vstatus(t, d, ctx, 1, cid, "topUpFeeReserve",
		fmt.Sprintf(`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1}}`, fundH, rawTx, proof))
	rec("MD06-PFR", "topUpFeeReserve of a user-deposit tx rejected (D-1)", !isOK(sBadFR), "status="+sBadFR)

	t.Logf("STAGE-6 SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}

// fundVaultCapture is fundVaultViaSPV but returns the proof pieces (for replay tests).
func fundVaultCapture(t *testing.T, d *Devnet, ctx context.Context, cid, primaryHex, backupHex, recipient string, sats int64, lastRelayed uint64) (uint64, string, string, string) {
	t.Helper()
	fundVaultViaSPV(t, d, ctx, cid, primaryHex, backupHex, recipient, sats, lastRelayed)
	// re-derive the last deposit's proof from the contract's current tip block
	h := contractLastHeight(t, d, ctx, cid)
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", bhash, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	json.Unmarshal([]byte(blockJSON), &blk)
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", blk.Tx[1])
	proof := reverseHexBytes(blk.Tx[0])
	return h, rawTx, proof, "deposit_to=" + recipient
}
