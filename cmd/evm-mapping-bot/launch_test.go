package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	transactionpool "vsc-node/modules/transaction-pool"

	gcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// submittedCalls decodes the contract calls the bot submitted.
func submittedCalls(t *testing.T, l2 *fakeL2) []transactionpool.VscContractCall {
	t.Helper()
	l2.mu.Lock()
	defer l2.mu.Unlock()
	var out []transactionpool.VscContractCall
	for _, b := range l2.submitted {
		var shell transactionpool.VSCTransactionShell
		if err := common.DecodeCbor(b, &shell); err != nil {
			t.Fatal(err)
		}
		for _, op := range shell.Tx {
			var c transactionpool.VscContractCall
			if err := common.DecodeCbor(op.Payload, &c); err != nil {
				t.Fatal(err)
			}
			c.RcLimit = shell.Headers.RcLimit
			out = append(out, c)
		}
	}
	return out
}

// RP-7: the bot derives MagiDeposit's address exactly as the bridge does
// (account-mapping TestRP7_DepositContractAddressIsTheCREATE2Address; cast).
func TestRP7_BotDepositAddressMatchesTheBridge(t *testing.T) {
	code, _ := hex.DecodeString(magiDepositCreationCode)
	if hex.EncodeToString(keccak256(code)) != magiDepositCreationCodeHash {
		t.Fatal("creation code does not hash to the pinned hash")
	}
	if got := depositContractAddress("0x88cbba6b5e5d2c20d9cfc55f3b2fc18d073ccf14"); got != "0x992421a176328dacaaa0f50ee5c840d49bd16dd7" {
		t.Fatalf("MagiDeposit for vault 0x88cb..cf14 = %s, want 0x992421a1..6dd7", got)
	}
}

// RP-7: a MagiDeposit token deposit is found, and the Transfer that paid it is
// marked so the ERC-20 scan does not also map it (the bridge would refuse it
// and the bot dead-letter it). A Transfer of another amount is not the payer.
func TestRP7_ScanFindsTheDepositAndItsPayingTransfer(t *testing.T) {
	vault := "0x88cbba6b5e5d2c20d9cfc55f3b2fc18d073ccf14"
	dc := depositContractAddress(vault)
	token := "0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"
	sender := "0x00000000000000000000000000000000000a11ce"
	pad := func(a string) string { return "0x" + strings.Repeat("0", 24) + strings.TrimPrefix(a, "0x") }
	word := func(v int64) string { return hex.EncodeToString(gcommon.LeftPadBytes(big.NewInt(v).Bytes(), 32)) }
	for _, tc := range []struct {
		paidAmount int64
		wantPaid   bool
	}{{7_000_000, true}, {6_999_999, false}} {
		logs := []map[string]any{
			{"address": token, "topics": []string{"0x" + TransferEventSig, pad(sender), pad(vault)}, "data": "0x" + word(tc.paidAmount), "logIndex": "0x4"},
			{"address": dc, "topics": []string{"0x" + depositEventSig, pad(sender), pad(token)}, "data": "0x" + word(7_000_000) + word(64) + word(8) + hex.EncodeToString([]byte("hive:bob")) + strings.Repeat("0", 48), "logIndex": "0x5"},
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var res any
			switch req.Method {
			case "eth_getLogs":
				res = []map[string]string{{"transactionIndex": "0x2", "transactionHash": "0xabc", "logIndex": "0x5"}}
			case "eth_getTransactionReceipt":
				res = map[string]any{"logs": logs}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": res})
		}))
		deps, paid, err := scanContractDeposits(newEthRPC(srv.URL), 105, dc, vault)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 1 || deps[0].DepositType != "contract" || deps[0].LogIndex != 1 || deps[0].TxIndex != 2 {
			t.Fatalf("deposits %+v, want one contract deposit at per-receipt log 1", deps)
		}
		if paid["0xabc#0"] != tc.wantPaid {
			t.Fatalf("Transfer of %d marked paid=%v, want %v", tc.paidAmount, paid["0xabc#0"], tc.wantPaid)
		}
	}
}

// RP-9: headerRLP re-encodes a header from its JSON, including Glamsterdam's
// fields (Sepolia), and checks it hashes to the block hash.
func TestRP9_HeaderRLPFromJSON(t *testing.T) {
	raw, err := os.ReadFile("testdata/headers.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx map[string]struct {
		RLP   string         `json:"rlp"`
		Block map[string]any `json:"block"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	for net, f := range fx {
		block := f.Block
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": block})
		}))
		got, err := headerRLP(newEthRPC(srv.URL), 1)
		if err != nil || "0x"+hex.EncodeToString(got) != f.RLP {
			t.Fatalf("%s: headerRLP err %v, matches %v", net, err, "0x"+hex.EncodeToString(got) == f.RLP)
		}
		// A field the bot does not know changes the hash: refused.
		block["slotNumber"], block["futureField"] = nil, "0x01"
		delete(block, "slotNumber")
		if net == "sepolia" {
			if _, err := headerRLP(newEthRPC(srv.URL), 1); err == nil {
				t.Fatalf("%s: a header missing a field was encoded", net)
			}
		}
		srv.Close()
	}
}

// RP-11: every in-flight withdrawal is broadcast, and mined ones are confirmed
// in nonce order, both in one tick.
func TestRP11_InFlightWithdrawalsBroadcastAndConfirmInOrder(t *testing.T) {
	tx0 := queuedTxNonce(t, 0)
	l1, l2, rpc, gql, cp, cfg := wdWorld(t, tx0)
	tx1 := queuedTxNonce(t, 1)
	l2.set("vsc1bridge", "np", "2")
	l2.set("vsc1bridge", "d-1", spendJSONNonce(tx1, 1))
	sh, _ := computeSighash(tx1)
	l2.sigs[sh] = tssSig(t, tx1)
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primary")
	if len(l1.sent) != 2 || cp.SentWithdrawals["0"].TxHash == "" || cp.SentWithdrawals["1"].TxHash == "" {
		t.Fatalf("broadcasts %d, sent %+v; want nonces 0 and 1", len(l1.sent), cp.SentWithdrawals)
	}
	// Both mine in block 101; the verifier stores its header.
	for _, s := range l1.sent {
		b, _ := hex.DecodeString(s[2:])
		var tx types.Transaction
		if err := tx.UnmarshalBinary(b); err != nil {
			t.Fatal(err)
		}
		c := l1.block(101)
		c.txs, c.receipts = append(c.txs, &tx), append(c.receipts, &types.Receipt{Type: types.DynamicFeeTxType, Status: 1, CumulativeGasUsed: 21000, TxHash: tx.Hash()})
	}
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "101")
	l2.status = func(string) string { return "UNCONFIRMED" }
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql, cfg: cfg}
	handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg)
	calls := submittedCalls(t, l2)
	if len(calls) != 2 || calls[0].Action != "confirmSpend" || calls[1].Action != "confirmSpend" ||
		!strings.Contains(calls[0].Payload, `"intent_nonce":0`) || !strings.Contains(calls[1].Payload, `"intent_nonce":1`) {
		t.Fatalf("calls %+v, want confirmSpend for nonce 0 then 1", calls)
	}
}

// RP-11: only the head is confirmed when an earlier withdrawal has not mined.
func TestRP11_ALaterWithdrawalWaitsForTheHead(t *testing.T) {
	l2, rpc, gql, cp, cfg, sub := confirmWorld(t)
	l2.set("vsc1bridge", "n", "0")
	head := cp.SentWithdrawals["0"]
	cp.SentWithdrawals["1"] = SentTx{TxHash: head.TxHash, Nonce: 1, SentAt: 1}
	head.TxHash = "0x" + strings.Repeat("ee", 32) // not mined
	cp.SentWithdrawals["0"] = head
	handleConfirmations(context.Background(), rpc, gql, sub, cp, cfg)
	if calls := submittedCalls(t, l2); len(calls) != 0 {
		t.Fatalf("confirmed %+v while nonce 0 has not mined", calls)
	}
}

// tipHeader is a 137-byte stored header with L1 timestamp ts.
func tipHeader(ts time.Time) string {
	hdr := make([]byte, 137)
	u := uint64(ts.Unix())
	for i := 0; i < 8; i++ {
		hdr[121+i] = byte(u >> (56 - 8*i))
	}
	return string(hdr)
}

// RP-11: the drain fills the free slots, with an RC limit sized to them, at
// most once a minute and only with a fresh price (else it would do nothing).
func TestRP11_DrainFillsTheFreeSlots(t *testing.T) {
	l2, sub := poolWorld(t)
	for k, v := range map[string]string{"wqh": "3", "wqt": "8", "n": "10", "np": "16", "zkverifier": "vsc1verifier"} {
		l2.set("vsc1bridge", k, v)
	}
	l2.set("vsc1verifier", "h", "500")
	l2.set("vsc1verifier", "b-500", tipHeader(time.Now().Add(-2*time.Hour)))
	lastDrainAt = time.Time{}
	drainIfQueued(context.Background(), l2.gql(t), sub, sub.cfg)
	if calls := submittedCalls(t, l2); len(calls) != 0 {
		t.Fatalf("drained on a 2 h old price: %+v", calls)
	}
	l2.set("vsc1verifier", "b-500", tipHeader(time.Now().Add(-5*time.Minute)))
	drainIfQueued(context.Background(), l2.gql(t), sub, sub.cfg)
	calls := submittedCalls(t, l2)
	if len(calls) != 1 || calls[0].Action != "drainWithdrawalQueue" || calls[0].Payload != `{"max":2}` || calls[0].RcLimit != 16_000 {
		t.Fatalf("calls %+v, want drainWithdrawalQueue max 2 (8 slots, 6 in flight) at 16k RC", calls)
	}
	drainIfQueued(context.Background(), l2.gql(t), sub, sub.cfg)
	if len(submittedCalls(t, l2)) != 1 {
		t.Fatal("drained twice within a minute")
	}
	lastDrainAt = time.Time{}
	l2.set("vsc1bridge", "np", "18") // full
	drainIfQueued(context.Background(), l2.gql(t), sub, sub.cfg)
	if len(submittedCalls(t, l2)) != 1 {
		t.Fatal("drained with every slot in flight")
	}
}

// RP-9: a submitted attestation or link run is not sent again until it had
// time to execute (it used to go out every 12 s tick until then).
func TestRP9_AttestationsAreNotResentEveryTick(t *testing.T) {
	l2, sub := poolWorld(t)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	cfg := sub.cfg
	cp := &Checkpoint{AttestCache: map[uint64]*attestEntry{900: {AccountProof: []string{"0xc0"},
		Headers: map[uint64]attestedBlock{850: {RLP: "c0", StorageProof: []string{"0xc0"}}}}}}
	gql := l2.gql(t)
	attestFromRoot(context.Background(), gql, sub, cp, cfg, "vsc1verifier", 900, []uint64{850})
	first := len(submittedCalls(t, l2))
	if first != 2 { // attestHistoryRoot + attestHeaders
		t.Fatalf("%d calls for one attestation, want 2", first)
	}
	attestFromRoot(context.Background(), gql, sub, cp, cfg, "vsc1verifier", 900, []uint64{850})
	if got := len(submittedCalls(t, l2)); got != first {
		t.Fatalf("the attestation went out again right away (%d calls)", got)
	}
	cp.AttestSentAt[900] = time.Now().Add(-attestResendAfter - time.Second).Unix()
	attestFromRoot(context.Background(), gql, sub, cp, cfg, "vsc1verifier", 900, []uint64{850})
	if got := len(submittedCalls(t, l2)); got != first+2 {
		t.Fatalf("not resent after %s (%d calls)", attestResendAfter, got)
	}
}

// RP-1: on mainnet a bot with no checkpoint needs START_BLOCK.
func TestRP1_MainnetNeedsAStartBlock(t *testing.T) {
	l2 := newFakeL2()
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "26000000")
	cp := &Checkpoint{}
	if err := startBlock(context.Background(), l2.gql(t), cp, EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-mainnet"}); err == nil || cp.LastScannedBlock != 0 {
		t.Fatalf("mainnet without START_BLOCK started at %d (%v)", cp.LastScannedBlock, err)
	}
}

// RP-1: a new checkpoint never starts at block 1.
func TestRP1_NewCheckpointStart(t *testing.T) {
	l2 := newFakeL2()
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	gql := l2.gql(t)
	cfg := EVMBotConfig{ContractID: "vsc1bridge"}
	cp := &Checkpoint{}
	if err := startBlock(context.Background(), gql, cp, cfg); err == nil {
		t.Fatal("started with no START_BLOCK and no verifier tip")
	}
	l2.set("vsc1verifier", "h", "12007500")
	if err := startBlock(context.Background(), gql, cp, cfg); err != nil || cp.LastScannedBlock != 12007500 {
		t.Fatalf("from the verifier tip: %v, checkpoint %d", err, cp.LastScannedBlock)
	}
	cp = &Checkpoint{}
	t.Setenv("START_BLOCK", "11000000")
	if err := startBlock(context.Background(), gql, cp, cfg); err != nil || cp.LastScannedBlock != 10999999 {
		t.Fatalf("START_BLOCK: %v, checkpoint %d", err, cp.LastScannedBlock)
	}
	cp.LastScannedBlock = 5
	if err := startBlock(context.Background(), gql, cp, cfg); err != nil || cp.LastScannedBlock != 5 {
		t.Fatal("an existing checkpoint was moved")
	}
}

// RP-5: an L1 RPC on another chain is refused.
func TestRP5_ChainIDMustMatchTheBridge(t *testing.T) {
	l2 := newFakeL2()
	l2.set("vsc1bridge", "chainid", "11155111")
	for chain, ok := range map[string]bool{"0xaa36a7": true, "0x1": false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": chain})
		}))
		err := checkChainID(context.Background(), newEthRPC(srv.URL), l2.gql(t), "vsc1bridge")
		srv.Close()
		if (err == nil) != ok {
			t.Fatalf("RPC chain %s: err %v, want ok=%v", chain, err, ok)
		}
	}
}

// RP-9: NEEDS_FILE asks for a proof while a needed header has no stored root
// above it, and while withdrawals wait on an old tip header.
func TestRP9_NeedsFileAsksForAProof(t *testing.T) {
	l2 := newFakeL2()
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	gql := l2.gql(t)
	path := t.TempDir() + "/needs.json"
	t.Setenv("NEEDS_FILE", path)
	cfg := EVMBotConfig{ContractID: "vsc1bridge"}
	read := func() map[string]any {
		var m map[string]any
		b, _ := os.ReadFile(path)
		_ = json.Unmarshal(b, &m)
		return m
	}
	cp := &Checkpoint{HeaderNeeds: map[uint64]int64{150: time.Now().Unix()}}
	writeNeedsFile(context.Background(), gql, cp, cfg, 140)
	if m := read(); m["want_proof"] != true {
		t.Fatalf("need above the tip: %v", m)
	}
	cp.HeaderNeeds = map[uint64]int64{120: 1}
	writeNeedsFile(context.Background(), gql, cp, cfg, 140)
	if m := read(); m["want_proof"] != false || cp.WantProofSince != 0 {
		t.Fatalf("need below the tip (attestable): %v", m)
	}
	// A queued withdrawal and a 2 h old tip header.
	hdr := make([]byte, 137)
	ts := uint64(time.Now().Add(-2 * time.Hour).Unix())
	for i := 0; i < 8; i++ {
		hdr[121+i] = byte(ts >> (56 - 8*i))
	}
	l2.set("vsc1verifier", "b-140", string(hdr))
	l2.set("vsc1bridge", "wqt", "1")
	writeNeedsFile(context.Background(), gql, cp, cfg, 140)
	if m := read(); m["want_proof"] != true {
		t.Fatalf("queued withdrawal on a 2 h old price: %v", m)
	}
}

func queuedTxNonce(t *testing.T, nonce uint64) string {
	t.Helper()
	b, err := gethrlp.EncodeToBytes([]any{big.NewInt(wireChainID), nonce, big.NewInt(2e9), big.NewInt(40e9), uint64(21000), wdTo, big.NewInt(1e18), []byte{}, types.AccessList{}})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(append([]byte{0x02}, b...))
}

func spendJSONNonce(unsignedHex string, nonce uint64) string {
	raw, _ := json.Marshal(contractPendingSpend{Nonce: nonce, Amount: big.NewInt(1e18), From: "hive:alice", To: wdTo.Hex(), Asset: "eth",
		UnsignedTxHex: unsignedHex, BlockHeight: 100, VaultAtQueue: "0x00000000000000000000000000000000000000fa"})
	_ = strconv.Itoa
	return string(raw)
}
