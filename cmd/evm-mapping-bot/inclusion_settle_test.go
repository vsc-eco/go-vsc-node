package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common/params"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// CONFIRM-RECEIPT-CAP-1: an ETH withdrawal whose receipt makes confirmSpend
// too large for the node is closed on its inclusion proof, then settled from
// its receipt.

type rcap1 struct {
	l1  *fakeL1
	l2  *fakeL2
	rpc *ethRPC
	gql *vscGraphQL
	sub *l2Submitter
	cp  *Checkpoint
	cfg EVMBotConfig
	blk *fakeChain
}

// rcap1World broadcasts withdrawal nonce 0 and mines it as the first
// transaction of L1 block 101, with a receipt of the given status and logs.
func rcap1World(t *testing.T, status uint64, logs ...*types.Log) *rcap1 {
	t.Helper()
	lastReceiptSettleAt = time.Time{}
	receiptSettleSent = map[string]time.Time{}
	l1, l2, rpc, gql, cp, cfg := wdWorld(t, queuedTxNonce(t, 0))
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
	if len(l1.sent) != 1 {
		t.Fatalf("broadcasts %d, want 1", len(l1.sent))
	}
	b, _ := hex.DecodeString(l1.sent[0][2:])
	var tx types.Transaction
	if err := tx.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	for i, l := range logs {
		l.TxHash, l.Index = tx.Hash(), uint(i)
	}
	rc := &types.Receipt{Type: types.DynamicFeeTxType, Status: status, CumulativeGasUsed: 90_000, Logs: logs, TxHash: tx.Hash()}
	rc.Bloom = types.CreateBloom(types.Receipts{rc})
	blk := l1.block(101)
	blk.txs, blk.receipts = append(blk.txs, &tx), append(blk.receipts, rc)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "101")
	l2.status = func(string) string { return "UNCONFIRMED" }
	l2.sim = func(string) map[string]any { return map[string]any{"success": true, "rc_used": 1187} }
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql, cfg: cfg}
	return &rcap1{l1: l1, l2: l2, rpc: rpc, gql: gql, sub: sub, cp: cp, cfg: cfg, blk: blk}
}

func (w *rcap1) calls(t *testing.T, action string) []string {
	t.Helper()
	var out []string
	for _, c := range submittedCalls(t, w.l2) {
		if c.Action == action {
			out = append(out, c.Payload)
		}
	}
	return out
}

func bigLog(to common.Address, n int) *types.Log {
	return &types.Log{Address: to, Data: make([]byte, n)}
}

// A recipient that logs 10 KB: the bot sends the confirm without the receipt,
// under the cap, and notes the withdrawal for its settlement; that receipt is
// too large to settle as well, so the note goes and nothing more is sent.
func TestRCAP1_OversizedETHConfirmClosesOnInclusion(t *testing.T) {
	w := rcap1World(t, 1, bigLog(wdTo, 10_000))
	handleConfirmations(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	got := w.calls(t, "confirmSpend")
	if len(got) != 1 {
		t.Fatalf("confirmSpend calls %d, want 1", len(got))
	}
	var req confirmSpendRequest
	if err := json.Unmarshal([]byte(got[0]), &req); err != nil {
		t.Fatal(err)
	}
	if req.ReceiptProof != "" || req.TxProof == "" || req.BlockHeight != 101 || req.TxIndex != 0 || req.IntentNonce != 0 {
		t.Fatalf("want an inclusion-only confirm of nonce 0 at 101/0, got %+v", req)
	}
	if n := nodePayloadSize([]byte(got[0])); n > params.MAX_CONTRACT_PAYLOAD_SIZE {
		t.Fatalf("inclusion-only confirm is %d bytes, over the cap", n)
	}
	if _, ok := w.cp.ReceiptSettles["wic-101-0"]; !ok {
		t.Fatalf("not noted for settlement: %v", w.cp.ReceiptSettles)
	}
	w.l2.set("vsc1bridge", "wic-101-0", `{"n":0}`)
	settleWithdrawalReceipts(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	if s := w.calls(t, "settleWithdrawalReceipt"); len(s) != 0 {
		t.Fatalf("an oversized receipt was sent to settle: %d calls", len(s))
	}
	if len(w.cp.ReceiptSettles) != 0 {
		t.Fatalf("an unsettleable record stays noted: %v", w.cp.ReceiptSettles)
	}
}

// A normal withdrawal is confirmed with its receipt, as before.
func TestRCAP1_NormalConfirmKeepsItsReceipt(t *testing.T) {
	w := rcap1World(t, 1)
	handleConfirmations(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	got := w.calls(t, "confirmSpend")
	if len(got) != 1 || strings.Contains(got[0], `"receipt_proof":""`) {
		t.Fatalf("want one confirm with its receipt, got %v", got)
	}
	if len(w.cp.ReceiptSettles) != 0 {
		t.Fatalf("a receipt confirm noted a settlement: %v", w.cp.ReceiptSettles)
	}
}

// A token withdrawal is never closed on inclusion (the bridge refuses it): an
// oversized one is not sent at all, as before.
func TestRCAP1_TokenWithdrawalIsNotClosedOnInclusion(t *testing.T) {
	w := rcap1World(t, 1, bigLog(wdTo, 10_000))
	raw, _ := json.Marshal(contractPendingSpend{Nonce: 0, Amount: big.NewInt(1e18), From: "hive:alice", To: wdTo.Hex(), Asset: "usdc",
		UnsignedTxHex: queuedTxNonce(t, 0), BlockHeight: 100, VaultAtQueue: wdVault.Hex()})
	w.l2.set("vsc1bridge", "d-0", string(raw))
	handleConfirmations(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	if got := w.calls(t, "confirmSpend"); len(got) != 0 {
		t.Fatalf("a token confirm was sent: %v", got)
	}
	if len(w.cp.ReceiptSettles) != 0 {
		t.Fatalf("a token withdrawal was noted: %v", w.cp.ReceiptSettles)
	}
}

// A noted close is settled with its own receipt, once per resend window, and
// forgotten when its record is gone.
func TestRCAP1_SettleSendsTheReceipt(t *testing.T) {
	w := rcap1World(t, 0)
	noteReceiptSettle(w.cp, 101, 0, 0)
	w.l2.set("vsc1bridge", "wic-101-0", `{"n":0}`)
	settleWithdrawalReceipts(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	got := w.calls(t, "settleWithdrawalReceipt")
	if len(got) != 1 {
		t.Fatalf("settle calls %d, want 1", len(got))
	}
	var p struct {
		BlockHeight  uint64 `json:"block_height"`
		TxIndex      uint64 `json:"tx_index"`
		ReceiptProof string `json:"receipt_proof"`
	}
	if err := json.Unmarshal([]byte(got[0]), &p); err != nil {
		t.Fatal(err)
	}
	if p.BlockHeight != 101 || p.TxIndex != 0 {
		t.Fatalf("settle names %d/%d, want 101/0", p.BlockHeight, p.TxIndex)
	}
	leaf, _ := w.blk.receipts[0].MarshalBinary()
	verifyLeafB64(t, "settle receipt", w.blk.receiptRoot(), 0, leaf, p.ReceiptProof)

	lastReceiptSettleAt = time.Time{}
	settleWithdrawalReceipts(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	if n := len(w.calls(t, "settleWithdrawalReceipt")); n != 1 {
		t.Fatalf("resent inside the resend window: %d calls", n)
	}
	w.l2.set("vsc1bridge", "wic-101-0", "")
	lastReceiptSettleAt = time.Time{}
	settleWithdrawalReceipts(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	if len(w.cp.ReceiptSettles) != 0 {
		t.Fatalf("settled record still noted: %v", w.cp.ReceiptSettles)
	}
}

// A close just sent whose record is not there yet stays noted.
func TestRCAP1_FreshNoteWaitsForItsRecord(t *testing.T) {
	w := rcap1World(t, 1)
	noteReceiptSettle(w.cp, 101, 0, 0)
	settleWithdrawalReceipts(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	if _, ok := w.cp.ReceiptSettles["wic-101-0"]; !ok {
		t.Fatal("a fresh note was dropped before its close executed")
	}
}

// Someone else closed the withdrawal on its inclusion proof: when the bot
// drops its entry (the nonce moved on) it notes the record for settlement.
func TestRCAP1_ThirdPartyCloseIsSettled(t *testing.T) {
	w := rcap1World(t, 0)
	w.l2.set("vsc1bridge", "n", "1")
	w.l2.set("vsc1bridge", "d-0", "")
	w.l2.set("vsc1bridge", "wic-101-0", `{"n":0}`)
	handleWithdrawals(context.Background(), w.rpc, w.gql, w.cp, w.cfg, "vsc1bridge-primary")
	if _, ok := w.cp.SentWithdrawals["0"]; ok {
		t.Fatal("closed withdrawal still tracked")
	}
	settleWithdrawalReceipts(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	if _, ok := w.cp.ReceiptSettles["wic-101-0"]; !ok {
		t.Fatalf("third-party close not noted: %v", w.cp.ReceiptSettles)
	}
	if len(w.cp.ClosedChecks) != 0 {
		t.Fatalf("check not cleared: %v", w.cp.ClosedChecks)
	}
	if got := w.calls(t, "settleWithdrawalReceipt"); len(got) != 1 {
		t.Fatalf("settle calls %d, want 1 (the noted close is settled in the same pass)", len(got))
	}
}

// A check whose hashes the RPC does not know is kept an hour (a mined
// withdrawal is found again), then dropped; checks are keyed by hash, so two
// generations' withdrawals at the same nonce are both kept.
func TestRCAP1_ClosedChecks(t *testing.T) {
	w := rcap1World(t, 0)
	w.cp.mu.Lock()
	w.cp.noteClosedCheckLocked(SentTx{TxHash: "0xaaaa", Nonce: 7})
	w.cp.noteClosedCheckLocked(SentTx{TxHash: "0xbbbb", Nonce: 7})
	w.cp.mu.Unlock()
	if len(w.cp.ClosedChecks) != 2 {
		t.Fatalf("checks %v: want both withdrawals at nonce 7", w.cp.ClosedChecks)
	}
	checkClosedWithdrawals(context.Background(), w.rpc, w.gql, w.cp, w.cfg)
	if len(w.cp.ClosedChecks) != 2 {
		t.Fatal("a fresh check of an unknown hash was dropped")
	}
	w.cp.mu.Lock()
	c := w.cp.ClosedChecks["0xaaaa"]
	c.Since = time.Now().Add(-2 * time.Hour).Unix()
	w.cp.ClosedChecks["0xaaaa"] = c
	w.cp.mu.Unlock()
	checkClosedWithdrawals(context.Background(), w.rpc, w.gql, w.cp, w.cfg)
	if _, ok := w.cp.ClosedChecks["0xaaaa"]; ok {
		t.Fatal("an unknown hash checked for over an hour was kept")
	}
	if _, ok := w.cp.ClosedChecks["0xbbbb"]; !ok {
		t.Fatal("the fresh check went with it")
	}
}

// A settlement the bridge refuses is not sent: it backs off (not counting
// toward the per-tick sends), so a new one is still sent at once; a refusal
// for a missing header asks for that header.
func TestRCAP1_RefusedSettleBacksOff(t *testing.T) {
	w := rcap1World(t, 0)
	key, _ := ethCrypto.GenerateKey()
	for n := uint64(90); n < 100; n++ {
		w.l1.block(n).addTx(t, key, &types.DynamicFeeTx{Nonce: n, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(2e9), Gas: 21000, To: &wdTo, Value: big.NewInt(1)})
		noteReceiptSettle(w.cp, n, 0, 0)
		w.l2.set("vsc1bridge", inclusionCloseKey(n, 0), `{"n":0}`)
	}
	w.l2.sim = func(string) map[string]any {
		return map[string]any{"success": false, "err": "intent_error", "err_msg": "block header not found"}
	}
	settleWithdrawalReceipts(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	if n := len(w.calls(t, "settleWithdrawalReceipt")); n != 0 {
		t.Fatalf("refused settlements were sent: %d", n)
	}
	for n := uint64(90); n < 100; n++ {
		r := w.cp.ReceiptSettles[inclusionCloseKey(n, 0)]
		if r.Refusals != 1 || r.NextAt <= time.Now().Unix() {
			t.Fatalf("block %d: refusals %d next %d, want one refusal and a backoff (all ten tried in one pass)", n, r.Refusals, r.NextAt)
		}
		if _, ok := w.cp.HeaderNeeds[n]; !ok {
			t.Fatalf("block %d: the header the bridge waits for was not asked for", n)
		}
	}
	// A new close is settled at once, the backed-off ones are not tried.
	w.l2.sim = func(string) map[string]any { return map[string]any{"success": true, "rc_used": 1187} }
	noteReceiptSettle(w.cp, 101, 0, 0)
	w.l2.set("vsc1bridge", "wic-101-0", `{"n":0}`)
	lastReceiptSettleAt = time.Time{}
	settleWithdrawalReceipts(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	got := w.calls(t, "settleWithdrawalReceipt")
	if len(got) != 1 || !strings.Contains(got[0], `"block_height":101`) {
		t.Fatalf("want only the new settlement sent, got %v", got)
	}
}

// With no dry run (GraphQL down) a settlement waits, and is not a refusal.
func TestRCAP1_SettleWaitsForItsDryRun(t *testing.T) {
	w := rcap1World(t, 0)
	w.l2.sim = nil
	noteReceiptSettle(w.cp, 101, 0, 0)
	w.l2.set("vsc1bridge", "wic-101-0", `{"n":0}`)
	settleWithdrawalReceipts(context.Background(), w.rpc, w.gql, w.sub, w.cp, w.cfg)
	if n := len(w.calls(t, "settleWithdrawalReceipt")); n != 0 {
		t.Fatalf("sent without a dry run: %d", n)
	}
	if r := w.cp.ReceiptSettles["wic-101-0"]; r.Refusals != 0 || r.NextAt != 0 {
		t.Fatalf("an unavailable dry run was counted as a refusal: %+v", r)
	}
}
