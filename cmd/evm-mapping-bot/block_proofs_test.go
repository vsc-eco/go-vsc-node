package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
)

// bp1Block is a real block's tx and receipt material: signed go-ethereum txs
// (legacy and dynamic-fee) and their receipts, with the roots go-ethereum's
// own DeriveSha gives, so the bot's encoders and trie are checked against the
// reference, not against themselves.
type bp1Block struct {
	height      uint64
	txs         types.Transactions
	receipts    types.Receipts
	txRoot      ethcommon.Hash
	receiptRoot ethcommon.Hash
}

func bp1MakeBlock(t *testing.T, height uint64, n int) *bp1Block {
	t.Helper()
	key, _ := ethCrypto.GenerateKey()
	signer := types.LatestSignerForChainID(big.NewInt(11155111))
	b := &bp1Block{height: height}
	var cum uint64
	for i := 0; i < n; i++ {
		to := ethcommon.BigToAddress(big.NewInt(int64(1000 + i)))
		var inner types.TxData
		if i%3 == 0 {
			inner = &types.LegacyTx{Nonce: uint64(i), GasPrice: big.NewInt(1e9), Gas: 21000, To: &to, Value: big.NewInt(int64(i + 1))}
		} else {
			inner = &types.DynamicFeeTx{ChainID: big.NewInt(11155111), Nonce: uint64(i), GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(3e9), Gas: 50000, To: &to, Value: big.NewInt(int64(i + 1)), Data: []byte{byte(i), 0, 0}}
		}
		tx, err := types.SignNewTx(key, signer, inner)
		if err != nil {
			t.Fatal(err)
		}
		cum += 21000 + uint64(i)
		r := &types.Receipt{Type: tx.Type(), Status: uint64(i % 2), CumulativeGasUsed: cum, TxHash: tx.Hash(), TransactionIndex: uint(i)}
		if i%4 == 1 {
			r.Logs = []*types.Log{{Address: to, Topics: []ethcommon.Hash{ethcommon.BigToHash(big.NewInt(int64(i)))}, Data: []byte{0, 0, byte(i)}}}
		}
		r.Bloom = types.CreateBloom(types.Receipts{r})
		b.txs = append(b.txs, tx)
		b.receipts = append(b.receipts, r)
	}
	b.txRoot = types.DeriveSha(b.txs, trie.NewStackTrie(nil))
	b.receiptRoot = types.DeriveSha(b.receipts, trie.NewStackTrie(nil))
	return b
}

// bp1RPC is a JSON-RPC endpoint serving one block. Each method can be made to
// refuse: blockReceipts=false answers eth_getBlockReceipts "method not found";
// fail429(method, k) answers HTTP 429 for that method's k-th call.
type bp1RPC struct {
	b             *bp1Block
	blockReceipts bool
	mu            sync.Mutex
	calls         map[string]int
	fail429       func(method string, k int) bool
	badReceipts   bool // eth_getBlockReceipts answers a wrong status for tx 0
	badRaw        bool // eth_getRawTransactionByHash answers tx 1's bytes for tx 0
}

func (s *bp1RPC) count(m string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[m]
}

func (s *bp1RPC) serve(t *testing.T) *ethRPC {
	t.Helper()
	s.calls = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.calls[req.Method]++
		k := s.calls[req.Method]
		s.mu.Unlock()
		if s.fail429 != nil && s.fail429(req.Method, k) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": v}) }
		receiptJSON := func(i int) json.RawMessage {
			raw, _ := s.b.receipts[i].MarshalJSON()
			return raw
		}
		switch req.Method {
		case "eth_getBlockByNumber":
			var full bool
			_ = json.Unmarshal(req.Params[1], &full)
			txs := make([]any, len(s.b.txs))
			for i, tx := range s.b.txs {
				if full {
					raw, _ := tx.MarshalJSON()
					txs[i] = json.RawMessage(raw)
				} else {
					txs[i] = tx.Hash().Hex()
				}
			}
			reply(map[string]any{"number": "0x" + strconv.FormatUint(s.b.height, 16), "transactions": txs, "transactionsRoot": s.b.txRoot.Hex(), "receiptsRoot": s.b.receiptRoot.Hex()})
		case "eth_getBlockReceipts":
			if !s.blockReceipts {
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "the method eth_getBlockReceipts does not exist"}})
				return
			}
			out := make([]json.RawMessage, len(s.b.receipts))
			for i := range s.b.receipts {
				out[i] = receiptJSON(i)
			}
			if s.badReceipts {
				var m map[string]any
				_ = json.Unmarshal(out[0], &m)
				m["status"] = "0x1"
				if s.b.receipts[0].Status == 1 {
					m["status"] = "0x0"
				}
				out[0], _ = json.Marshal(m)
			}
			reply(out)
		case "eth_getTransactionReceipt":
			var h string
			_ = json.Unmarshal(req.Params[0], &h)
			for i, tx := range s.b.txs {
				if strings.EqualFold(tx.Hash().Hex(), h) {
					reply(receiptJSON(i))
					return
				}
			}
			reply(nil)
		case "eth_getRawTransactionByHash":
			var h string
			_ = json.Unmarshal(req.Params[0], &h)
			for i, tx := range s.b.txs {
				if strings.EqualFold(tx.Hash().Hex(), h) {
					if s.badRaw && i == 0 {
						tx = s.b.txs[1]
					}
					raw, _ := tx.MarshalBinary()
					reply(fmt.Sprintf("0x%x", raw))
					return
				}
			}
			reply(nil)
		default:
			reply(nil)
		}
	}))
	t.Cleanup(srv.Close)
	return newEthRPC(srv.URL)
}

func bp1ResetCache(t *testing.T) {
	t.Helper()
	blockProofCache.Lock()
	blockProofCache.m = map[uint64]*blockProofs{}
	blockProofCache.order = nil
	blockProofCache.Unlock()
	t.Cleanup(func() {
		blockProofCache.Lock()
		blockProofCache.m = map[uint64]*blockProofs{}
		blockProofCache.order = nil
		blockProofCache.Unlock()
	})
}

// bp1Check asserts bp proves every tx and receipt against go-ethereum's roots:
// the bot's encodings are the consensus bytes and its trie gives the root.
func bp1Check(t *testing.T, b *bp1Block, bp *blockProofs, raw bool) {
	t.Helper()
	if got := listRoot(bp.receipts); !bytes.Equal(got, b.receiptRoot.Bytes()) {
		t.Fatalf("receipts root %x, go-ethereum %x", got, b.receiptRoot)
	}
	if raw {
		if got := listRoot(bp.raw); !bytes.Equal(got, b.txRoot.Bytes()) {
			t.Fatalf("tx root %x, go-ethereum %x", got, b.txRoot)
		}
		for i, tx := range b.txs {
			want, _ := tx.MarshalBinary()
			if !bytes.Equal(bp.raw[i], want) {
				t.Fatalf("raw tx %d differs from its consensus bytes", i)
			}
		}
	}
}

// BP-1: an endpoint that serves eth_getBlockReceipts gives a 300-tx block's
// receipts in one call (was 300 per-tx calls); an ERC-20 deposit's proof
// (receipts only) is 2 calls instead of 301, an ETH deposit's 302 instead of 601.
func TestBP1_ReceiptsInOneCall(t *testing.T) {
	bp1ResetCache(t)
	b := bp1MakeBlock(t, 12059266, 300)
	s := &bp1RPC{b: b, blockReceipts: true}
	rpc := s.serve(t)
	bp, err := fetchBlockProofs(context.Background(), rpc, b.height, false)
	if err != nil {
		t.Fatal(err)
	}
	bp1Check(t, b, bp, false)
	if s.count("eth_getTransactionReceipt") != 0 || s.count("eth_getBlockByNumber") != 1 || s.count("eth_getBlockReceipts") != 1 {
		t.Fatalf("calls %v, want 1 block + 1 block-receipts", s.calls)
	}
	bp1ResetCache(t)
	bp, err = fetchBlockProofs(context.Background(), rpc, b.height, true)
	if err != nil {
		t.Fatal(err)
	}
	bp1Check(t, b, bp, true)
	if s.count("eth_getTransactionReceipt") != 0 || s.count("eth_getRawTransactionByHash") != 300 {
		t.Fatalf("calls %v, want the receipts in one call and 300 raw txs", s.calls)
	}
}

// BP-1: an endpoint without eth_getBlockReceipts still works through the
// per-tx path, and the result is the same bytes.
func TestBP1_NoBlockReceiptsFallsBackPerTx(t *testing.T) {
	bp1ResetCache(t)
	b := bp1MakeBlock(t, 100, 40)
	s := &bp1RPC{b: b, blockReceipts: false}
	rpc := s.serve(t)
	bp, err := fetchBlockProofs(context.Background(), rpc, b.height, false)
	if err != nil {
		t.Fatal(err)
	}
	bp1Check(t, b, bp, false)
	if s.count("eth_getTransactionReceipt") != 40 {
		t.Fatalf("per-tx receipts %d, want 40", s.count("eth_getTransactionReceipt"))
	}
}

// BP-1, the testnet failure: per-tx path, a throttled endpoint refuses calls.
// Before: one refused call threw the block away and the next tick started at
// tx 0, so a block whose calls kept getting refused never finished. Now each
// try keeps what it fetched: the block completes, and the total number of
// per-tx calls is the block's txs plus the refused ones, not a multiple.
func TestBP1_ThrottledEndpointKeepsProgress(t *testing.T) {
	bp1ResetCache(t)
	old := callRetryPauses
	callRetryPauses = nil // a refused call fails the try at once
	t.Cleanup(func() { callRetryPauses = old })
	const n = 200
	b := bp1MakeBlock(t, 12059267, n)
	s := &bp1RPC{b: b, blockReceipts: false}
	// Every 37th receipt call and every 41st raw call is refused.
	s.fail429 = func(m string, k int) bool {
		return (m == "eth_getTransactionReceipt" && k%37 == 0) || (m == "eth_getRawTransactionByHash" && k%41 == 0)
	}
	rpc := s.serve(t)
	var bp *blockProofs
	tries := 0
	for ; tries < 50; tries++ {
		var err error
		if bp, err = fetchBlockProofs(context.Background(), rpc, b.height, true); err == nil {
			break
		}
	}
	if bp == nil {
		t.Fatalf("never completed in %d tries", tries)
	}
	bp1Check(t, b, bp, true)
	if got := s.count("eth_getTransactionReceipt"); got > n+n/37+8 {
		t.Fatalf("%d receipt calls for %d txs: fetched receipts were not kept", got, n)
	}
	if got := s.count("eth_getRawTransactionByHash"); got > n+n/41+8 {
		t.Fatalf("%d raw calls for %d txs: fetched raw txs were not kept", got, n)
	}
	t.Logf("completed after %d failed tries: %d receipt calls, %d raw calls for %d txs", tries, s.count("eth_getTransactionReceipt"), s.count("eth_getRawTransactionByHash"), n)
}

// BP-1: a block-receipts answer that does not rebuild the header's root is
// not used (the per-tx path supplies the right receipts).
func TestBP1_WrongBlockReceiptsAreNotUsed(t *testing.T) {
	bp1ResetCache(t)
	b := bp1MakeBlock(t, 7, 12)
	s := &bp1RPC{b: b, blockReceipts: true, badReceipts: true}
	rpc := s.serve(t)
	bp, err := fetchBlockProofs(context.Background(), rpc, b.height, false)
	if err != nil {
		t.Fatal(err)
	}
	bp1Check(t, b, bp, false)
	if s.count("eth_getTransactionReceipt") != 12 {
		t.Fatalf("the per-tx path did not replace the wrong answer: %d calls", s.count("eth_getTransactionReceipt"))
	}
}

// BP-1: if nothing rebuilds the header's root, no proof comes out (the bot
// never submits a map the contract would reject) and nothing wrong is kept.
func TestBP1_NoProofWithoutTheHeaderRoot(t *testing.T) {
	bp1ResetCache(t)
	b := bp1MakeBlock(t, 9, 6)
	b.receiptRoot = ethcommon.HexToHash("0x1234") // the header commits to something else
	s := &bp1RPC{b: b, blockReceipts: true}
	rpc := s.serve(t)
	if _, err := fetchBlockProofs(context.Background(), rpc, b.height, false); err == nil || !strings.Contains(err.Error(), "receipts root") {
		t.Fatalf("err = %v, want a receipts root mismatch", err)
	}
	blockProofCache.Lock()
	_, kept := blockProofCache.m[b.height]
	blockProofCache.Unlock()
	if kept {
		t.Fatal("the wrong material stayed cached")
	}
}

// BP-1: a block that changed under the same height (a reorg) starts over
// instead of mixing the old block's receipts in.
func TestBP1_ReorgedBlockStartsOver(t *testing.T) {
	bp1ResetCache(t)
	b1 := bp1MakeBlock(t, 55, 10)
	s1 := &bp1RPC{b: b1, blockReceipts: true}
	if _, err := fetchBlockProofs(context.Background(), s1.serve(t), 55, false); err != nil {
		t.Fatal(err)
	}
	b2 := bp1MakeBlock(t, 55, 11)
	s2 := &bp1RPC{b: b2, blockReceipts: true}
	bp, err := fetchBlockProofs(context.Background(), s2.serve(t), 55, false)
	if err != nil {
		t.Fatal(err)
	}
	bp1Check(t, b2, bp, false)
}

// BP-1: raw txs that do not rebuild the header's transactionsRoot give no
// proof (an ETH deposit's tx proof would be rejected on chain) and are not kept.
func TestBP1_WrongRawTxNoProof(t *testing.T) {
	bp1ResetCache(t)
	b := bp1MakeBlock(t, 11, 8)
	s := &bp1RPC{b: b, blockReceipts: true, badRaw: true}
	rpc := s.serve(t)
	if _, err := fetchBlockProofs(context.Background(), rpc, b.height, true); err == nil || !strings.Contains(err.Error(), "transactions root") {
		t.Fatalf("err = %v, want a transactions root mismatch", err)
	}
	blockProofCache.Lock()
	_, kept := blockProofCache.m[b.height]
	blockProofCache.Unlock()
	if kept {
		t.Fatal("the wrong raw txs stayed cached")
	}
}
