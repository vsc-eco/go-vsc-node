package devnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// simulateRcUsed dry-runs one contract call via the node's simulateContractCalls endpoint (the same
// endpoint mapping-bot's sizeCallByDryRun uses) and returns the RC it would consume. SimulateContractCallResult
// exposes rc_used:Int64! and gas_used:Uint64!. Mirrors vfSimulate but also pulls those two numbers.
func simulateRcUsed(d *Devnet, ctx context.Context, node int, cid, caller, action, payload string, rcCap uint64) (rcUsed int64, gasUsed uint64, ok bool, msg string) {
	var out struct {
		Sim []struct {
			Success bool    `json:"success"`
			Err     *string `json:"err"`
			ErrMsg  *string `json:"err_msg"`
			RcUsed  int64   `json:"rc_used"`
			GasUsed uint64  `json:"gas_used"`
		} `json:"simulateContractCalls"`
	}
	err := d.gqlQuery(ctx, node, `query($i: SimulateContractCallsInput!){ simulateContractCalls(input: $i){ success err err_msg rc_used gas_used } }`,
		map[string]any{"i": map[string]any{
			"tx_id":          fmt.Sprintf("devnet-dryrun-%d", time.Now().UnixNano()), // wall: unique id
			"required_auths": []string{caller},
			"calls": []map[string]any{{
				"contract_id": cid, "action": action, "payload": payload, "rc_limit": rcCap,
			}},
		}}, &out)
	if err != nil || len(out.Sim) != 1 {
		return 0, 0, false, fmt.Sprintf("simulate unavailable: %v", err)
	}
	m := ""
	if out.Sim[0].ErrMsg != nil {
		m = *out.Sim[0].ErrMsg
	} else if out.Sim[0].Err != nil {
		m = *out.Sim[0].Err
	}
	return out.Sim[0].RcUsed, out.Sim[0].GasUsed, out.Sim[0].Success, m
}

// settleTrancheSweepMeasured is a FILE-PRIVATE copy of settleSweepByTxid (vault_stage4_test.go) that,
// right before the real confirmSpend, DRY-RUNS the same confirmSpend through simulateContractCalls to
// MEASURE rc_used at the MaxMigrationInputs tranche size. It records the real n-input cost next to 2x
// (what d3bf0679's sizeCallByDryRun would send, with no balance clamp) and the anchors (17-input =
// 16,752 RC; bot DID RC = 118,374). It does NOT run the bot -- it measures through the bot's own
// dry-run endpoint. The confirmSpend RC is charged to the caller, but rc_used is the execution cost
// regardless of payer. Hard asserts (settle + drain) stay in the caller; the measurement is logged.
func settleTrancheSweepMeasured(t *testing.T, d *Devnet, ctx context.Context, cid, retiringKeyId, txid string, c *vfCase) string {
	t.Helper()
	sd := waitSigningData(t, d, ctx, cid, txid)
	if sd == nil {
		settleReport(t, "CASE TRANCHE-SETTLE FAIL — no signing data for sweep %s", txid)
		return "NO_SIGNING_DATA"
	}
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		settleReport(t, "deser sweep: %v", err)
		return "DESERIALIZE_FAILED"
	}
	for _, uh := range sd.UnsignedSigHashes {
		sig := waitSignature(t, d, ctx, retiringKeyId, uh.SigHash)
		if sig == nil {
			settleReport(t, "CASE TRANCHE-SETTLE — retiring gen did NOT sign input %d", uh.Index)
			return "NOT_SIGNED"
		}
		signature := append(append([]byte{}, sig...), byte(txscript.SigHashAll))
		mtx.TxIn[uh.Index].Witness = wire.TxWitness{signature, []byte{0x01}, uh.WitnessScript}
	}
	var buf bytes.Buffer
	mtx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding)
	bcTxid, err := d.bitcoinCli(ctx, "sendrawtransaction", hex.EncodeToString(buf.Bytes()))
	if err != nil {
		settleReport(t, "CASE TRANCHE-SETTLE FAIL — sweep broadcast rejected: %v", err)
		return "BROADCAST_REJECTED"
	}
	h, _ := d.MineBlocks(ctx, 1)
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
		settleReport(t, "CASE TRANCHE-SETTLE FAIL: sweep block %d unreadable (%d txs, ctx %v)", h, len(blk.Tx), ctx.Err())
		return "BLOCK_UNREADABLE"
	}
	// (i) 2026-10-14: raw_tx_hex must match what the bot's confirmSpend actually sends — the
	// NON-WITNESS serialization. The bot stores dbTx.RawTx as the UNSIGNED tx (AddPendingTransaction,
	// mapping-bot database/state.go:157) and never writes the witness form back; the contract verifies
	// via tx.TxHash() (proof.go:49), which is witness-independent. getrawtransaction returns the
	// witness-serialized MINED tx (~30k hex for 70 P2WSH inputs) which blows Hive's 8192B custom_json
	// cap (the earlier TRANCHE-SETTLE FAIL was this test-harness bug, NOT an NI-4 insufficiency);
	// mtx.SerializeNoWitness is ~5846 hex at 70 and is exactly the production form.
	var nwBuf bytes.Buffer
	if err := mtx.SerializeNoWitness(&nwBuf); err != nil {
		settleReport(t, "CASE TRANCHE-SETTLE FAIL — non-witness serialize: %v", err)
		return "SERIALIZE_FAILED"
	}
	rawTx := hex.EncodeToString(nwBuf.Bytes())
	proof := reverseHexBytes(blk.Tx[0])
	csPayload := fmt.Sprintf(`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"indices":[0]}`, h, rawTx, proof)

	// MEASURE: dry-run the confirmSpend through the bot's OWN cap to land in the right branch.
	caller := "hive:" + d.witnessAccount(1)
	n := len(sd.UnsignedSigHashes)
	const simulateRcCap = 100_000 // mapping-bot sizeCallByDryRun cap (call_contract_l2.go:151)
	const botDidRc = 118374       // TESTNET bot DID full RC. MAINNET bot balance is a separate owner number.
	rcUsed, gasUsed, simOK, simMsg := simulateRcUsed(d, ctx, 1, cid, caller, "confirmSpend", csPayload, simulateRcCap)
	// If it hit the cap (rc_used>100k -> gas_limit_hit), re-measure at a high cap for the TRUE magnitude.
	rcTrue := rcUsed
	if !simOK {
		if rt, _, ok2, _ := simulateRcUsed(d, ctx, 1, cid, caller, "confirmSpend", csPayload, 8_000_000); ok2 && rt > rcTrue {
			rcTrue = rt
		}
	}
	twoX := rcTrue * 2
	// 3-way verdict matching the bot exactly:
	var verdict string
	switch {
	case simOK && twoX <= botDidRc:
		verdict = fmt.Sprintf("(a) OK: rc_used=%d<=100k, bot sends 2x=%d <= botRC=%d", rcUsed, twoX, botDidRc)
	case simOK && twoX > botDidRc:
		verdict = fmt.Sprintf("(b) LOCAL REFUSAL: rc_used=%d<=100k, bot sends UNCLAMPED 2x=%d > botRC=%d -> node 'not enough RCS' -> full-tranche confirmSpend can NEVER submit until bot funded", rcUsed, twoX, botDidRc)
	case !simOK && strings.Contains(simMsg, "gas_limit_hit"):
		verdict = fmt.Sprintf("(c) rc_used(true)=%d >100k (gas_limit_hit) -> sizeAboveDryRunCap; if botRC(%d)<=100k it falls back to CONFIGURED 10k -> submits -> FAILS ON CHAIN (drains RC)", rcTrue, botDidRc)
	default:
		verdict = fmt.Sprintf("(?) dry-run failed, not gas_limit_hit: msg=%q (inspect; true_rc@8M=%d)", simMsg, rcTrue)
	}
	t.Logf("B3b MEASURE confirmSpend n=%d @cap100k: rc_used=%d gas_used=%d ok=%v msg=%q | true_rc_used=%d 2x=%d | anchors 17in=16752 testnet_botRC=%d | VERDICT %s",
		n, rcUsed, gasUsed, simOK, simMsg, rcTrue, twoX, botDidRc, verdict)
	if c != nil {
		c.rec("TRANCHE-DRYRUN-RC",
			"MEASURED confirmSpend rc_used at MaxMigrationInputs via the bot's own dry-run (cap 100k): decides if d3bf0679's 2x exceeds the bot RC (strand) or >100k falls back to 10k (on-chain fail)",
			rcUsed > 0 || rcTrue > 0,
			fmt.Sprintf("n=%d rc_used@100k=%d gas_used=%d ok=%v msg=%q true_rc_used=%d 2x=%d testnet_botRC=%d anchor_17in=16752 VERDICT=%s", n, rcUsed, gasUsed, simOK, simMsg, rcTrue, twoX, botDidRc, verdict))
	}

	cs := vstatus(t, d, ctx, 1, cid, "confirmSpend", csPayload)
	if isOK(cs) {
		t.Logf("TRANCHE-SETTLE — MaxMigrationInputs sweep SETTLED via confirmSpend (gen-0 drained)")
	} else {
		settleReport(t, "CASE TRANCHE-SETTLE FAIL: sweep %s did not settle (confirmSpend status=%s)", bcTxid, cs)
	}
	return cs
}
