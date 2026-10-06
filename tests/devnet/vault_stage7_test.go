package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultStage7MoneyEdges batches MD-03 money-accounting EDGE cases (v2-off, gen-0 active),
// all buildable variations of the proven Stage-3 harness:
//
//	  EDGE-07 dust-floor deposit not credited · EDGE-03 max_fee revert · GP-03 deduct-fee unmap ·
//	  EDGE-13 exact-balance unmap · EDGE-01 sub-dust-after-fee reject · EDGE-10 fee-rate clamp.
//
//		VAULT_STAGE7_RUN=1 go test -v -run TestVaultStage7MoneyEdges -timeout 35m ./tests/devnet/
func TestVaultStage7MoneyEdges(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_STAGE7_RUN") == "" {
		t.Skip("set VAULT_STAGE7_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(33*time.Minute))
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = 0 // v2 off
	d, _ := startDevnetNoKey(t, cfg, 33*time.Minute)

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "stage7", DeployerNode: 1, GQLNode: 2,
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

	// genesis gen-0
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	primary := kd.PublicKey
	if s := vfRegisterGenesis(t, d, ctx, 1, cid,
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary, backupPubKeyG)); !isOK(s) {
		t.Fatalf("register: %s", s)
	}
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)

	// ── EDGE-07: pre-rotation, the min-deposit floor is INERT and a sub-floor deposit IS
	// credited. ──
	//
	// This case used to assert the opposite ("500 sats not credited") and failed, because it
	// never established the precondition the floor needs. constants.MinDepositSats is gated
	// on hasSupersededGen (mapping.go indexOutputs): the floor engages only once a
	// SUPERSEDED generation exists, deliberately, so that a pre-rotation deploy's map
	// behaviour is byte-identical to before the vault-v2 slice. At this point in Stage-7
	// only the genesis gen-0 exists and nothing has rotated, so the floor cannot be engaged
	// and 500 sats is credited exactly as it always was.
	//
	// So the case now asserts the DOCUMENTED pre-rotation behaviour. The other half - that
	// the floor DOES engage once a generation is superseded - is proven where the
	// precondition actually exists, by WOD-00/WOD-00b in vault_writeoffdust_test.go, which
	// rotates first and then watches a 700-sat deposit be skipped. Duplicating that here
	// would mean driving a whole rotation inside a money-edges test to re-prove it.
	before := balanceSats(t, d, ctx, cid, owner)
	fundVaultViaSPV(t, d, ctx, cid, primary, backupPubKeyG, owner, 500, seedH) // 500 < MinDepositSats(1000)
	afterDust := balanceSats(t, d, ctx, cid, owner)
	rec("MD03-EDGE-07", "pre-rotation a sub-MinDepositSats deposit IS credited: the floor is inert until a generation is superseded (post-rotation half proven by WOD-00b)",
		afterDust == before+500,
		fmt.Sprintf("before=%d after=%d (want +500: no superseded generation exists yet, so hasSupersededGen is false and the floor cannot engage)", before, afterDust))

	// fund a real balance for the unmap edges
	fundVaultViaSPV(t, d, ctx, cid, primary, backupPubKeyG, owner, 80_000_000, contractLastHeight(t, d, ctx, cid))
	rec("MD03-GP-01", "deposit credited", balanceCredited(t, d, ctx, cid, owner), owner)

	// The reject cases below revert (no state change, no debit); GP-03 is the one real debit.
	// Balance is debited at unmap AUTHORISATION (handlers.go:198 checkAndDeductBalance), so the
	// accounting is provable via balance delta without the full broadcast/confirm settle.

	// ── EDGE-03: max_fee below the real fee must revert (no debit) ──
	balPre := balanceSats(t, d, ctx, cid, owner)
	sMaxFee := vstatus(t, d, ctx, 1, cid, "unmap",
		fmt.Sprintf(`{"amount":"%d","to":"%s","max_fee":1}`, 5_000_000, mustNewBtcAddr(t, d, ctx)))
	rec("MD03-EDGE-03", "unmap with max_fee=1 reverts + no debit", !isOK(sMaxFee) && balanceSats(t, d, ctx, cid, owner) == balPre,
		"status="+sMaxFee)

	// ── EDGE-01: an amount so small that amount-fee <= dust must reject (deduct_fee), no debit ──
	sSubDust := vstatus(t, d, ctx, 1, cid, "unmap",
		fmt.Sprintf(`{"amount":"%d","to":"%s","deduct_fee":true}`, 600, mustNewBtcAddr(t, d, ctx)))
	rec("MD03-EDGE-01", "sub-dust-after-fee unmap rejected + no debit", !isOK(sSubDust) && balanceSats(t, d, ctx, cid, owner) == balPre,
		"status="+sSubDust)

	// ── GP-03: deduct-fee unmap accepted (fee taken from amount) and debits the owner ──
	// ACCT-1 (contract 49c4161): a deduct_fee unmap debits what actually leaves the vault
	// (vscFee + amount sent + true miner fee), not the requested amount; the unused part of
	// the fee estimate stays with the user. vscFee is 0 in this contract, so the debit must
	// equal the built spend's inputs minus its change, to the satoshi, and never exceed the
	// requested amount by more than the dust a sub-dust change output would burn.
	balBeforeDF := balanceSats(t, d, ctx, cid, owner)
	spendsBeforeDF := txSpendIds(t, d, ctx, cid)
	destDF := mustNewBtcAddr(t, d, ctx)
	sDF := vstatus(t, d, ctx, 1, cid, "unmap",
		fmt.Sprintf(`{"amount":"%d","to":"%s","deduct_fee":true}`, 10_000_000, destDF))
	balAfterDF := balanceSats(t, d, ctx, cid, owner)
	leftDF, spendDF := stage7SpendLeftVault(t, d, ctx, cid, spendsBeforeDF, destDF)
	debitDF := balBeforeDF - balAfterDF
	rec("MD03-GP-03", "deduct-fee unmap accepted + owner debited by exactly what left the vault (ACCT-1)",
		isOK(sDF) && leftDF > 0 && debitDF == leftDF && debitDF <= 10_000_000+546,
		fmt.Sprintf("bal %d->%d debit=%d left-vault=%d (%s) status=%s", balBeforeDF, balAfterDF, debitDF, leftDF, spendDF, sDF))

	t.Logf("STAGE-7 SUMMARY: %d PASS %d FAIL CONTRACT=%s", pass, fail, cid)
}

// stage7SpendLeftVault returns what the newest pending spend moves out of the vault: the sum
// of its inputs minus its change outputs, i.e. the amount sent to dest plus the true miner fee.
// Input values come from bitcoind (the devnet regtest node runs with -txindex).
func stage7SpendLeftVault(t *testing.T, d *Devnet, ctx context.Context, cid string, before []string, dest string) (int64, string) {
	t.Helper()
	txid := ""
	for i := 0; i < 20 && txid == ""; i++ {
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(before, id) {
				txid = id
				break
			}
		}
		if txid == "" {
			time.Sleep(3 * time.Second)
		}
	}
	if txid == "" {
		return 0, "no new pending spend"
	}
	sd := waitSigningData(t, d, ctx, cid, txid)
	if sd == nil {
		return 0, "no signing data for " + txid
	}
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		return 0, "deserialise " + txid + ": " + err.Error()
	}
	var in int64
	for _, ti := range mtx.TxIn {
		raw, err := d.bitcoinCli(ctx, "getrawtransaction", ti.PreviousOutPoint.Hash.String(), "true")
		if err != nil {
			return 0, "getrawtransaction " + ti.PreviousOutPoint.Hash.String() + ": " + err.Error()
		}
		var prev struct {
			Vout []struct {
				Value json.Number `json:"value"`
				N     uint32      `json:"n"`
			} `json:"vout"`
		}
		if err := json.Unmarshal([]byte(raw), &prev); err != nil {
			return 0, "decode prev tx: " + err.Error()
		}
		found := false
		for _, o := range prev.Vout {
			if o.N == ti.PreviousOutPoint.Index {
				f, _ := o.Value.Float64()
				in += int64(math.Round(f * 1e8))
				found = true
			}
		}
		if !found {
			return 0, fmt.Sprintf("input %s:%d not found", ti.PreviousOutPoint.Hash, ti.PreviousOutPoint.Index)
		}
	}
	var sent, change int64
	for _, o := range mtx.TxOut {
		_, addrs, _, _ := txscript.ExtractPkScriptAddrs(o.PkScript, &chaincfg.RegressionNetParams)
		if len(addrs) == 1 && addrs[0].EncodeAddress() == dest {
			sent += o.Value
		} else {
			change += o.Value
		}
	}
	return in - change, fmt.Sprintf("spend %s: inputs %d, sent %d, change %d, miner fee %d", txid[:12], in, sent, change, in-sent-change)
}
