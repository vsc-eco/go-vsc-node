package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// fakeL1 is an Ethereum RPC over several blocks, each a fakeChain, plus a
// mempool of broadcast transactions that mine only when the test says so.
type fakeL1 struct {
	mu        sync.Mutex
	blocks    map[uint64]*fakeChain
	finalized uint64
	sent      []string // raw signed txs passed to eth_sendRawTransaction
	balances  map[string]*big.Int // eth_getBalance by lowercase address (nil: 0)
}

func newFakeL1() *fakeL1 { return &fakeL1{blocks: map[uint64]*fakeChain{}} }

func (f *fakeL1) block(n uint64) *fakeChain {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocks[n] == nil {
		f.blocks[n] = &fakeChain{}
	}
	if n > f.finalized {
		f.finalized = n
	}
	return f.blocks[n]
}

func (f *fakeL1) rpc(t *testing.T) *ethRPC {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var arg0 string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &arg0)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var result any
		switch req.Method {
		case "eth_getBlockByNumber":
			n := f.finalized
			if arg0 != "finalized" && arg0 != "latest" {
				n, _ = hexutil.DecodeUint64(arg0)
			}
			c := f.blocks[n]
			var full bool
			if len(req.Params) > 1 {
				_ = json.Unmarshal(req.Params[1], &full)
			}
			if c == nil {
				c = &fakeChain{}
			}
			if !full {
				hashes := make([]string, len(c.txs))
				for i, tx := range c.txs {
					hashes[i] = tx.Hash().Hex()
				}
				result = map[string]any{"number": hexutil.EncodeUint64(n), "transactions": hashes}
				break
			}
			txs := make([]map[string]string, len(c.txs))
			for i, tx := range c.txs {
				from, _ := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
				txs[i] = map[string]string{"hash": tx.Hash().Hex(), "from": from.Hex(), "to": tx.To().Hex(), "value": hexutil.EncodeBig(tx.Value())}
			}
			result = map[string]any{"number": hexutil.EncodeUint64(n), "transactions": txs}
		case "eth_getLogs":
			result = []any{}
		case "eth_getRawTransactionByHash", "eth_getTransactionByHash", "eth_getTransactionReceipt":
			for n, c := range f.blocks {
				for i, tx := range c.txs {
					if tx.Hash().Hex() != arg0 {
						continue
					}
					switch req.Method {
					case "eth_getRawTransactionByHash":
						b, _ := tx.MarshalBinary()
						result = hexutil.Encode(b)
					case "eth_getTransactionByHash":
						result = map[string]any{"hash": tx.Hash().Hex()}
					default:
						rc := *c.receipts[i]
						rc.BlockNumber = new(big.Int).SetUint64(n)
						result = &rc
					}
				}
			}
		case "eth_getBalance":
			b := f.balances[strings.ToLower(arg0)]
			if b == nil {
				b = new(big.Int)
			}
			result = hexutil.EncodeBig(b)
		case "eth_sendRawTransaction":
			f.sent = append(f.sent, arg0)
			b, _ := hexutil.Decode(arg0)
			var tx types.Transaction
			if err := tx.UnmarshalBinary(b); err != nil {
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32000, "message": err.Error()}})
				return
			}
			result = tx.Hash().Hex()
		default:
			t.Errorf("fakeL1: unexpected method %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(srv.Close)
	return newEthRPC(srv.URL)
}

// fakeL2 serves the Magi GraphQL calls the bot makes: contract state, TSS
// signatures, the account nonce, transaction submission and status.
type fakeL2 struct {
	mu        sync.Mutex
	state     map[string]map[string]string // contract id -> key -> value
	sigs      map[string]string            // sighash hex -> DER hex
	submitted [][]byte                     // signed L2 txs, decoded from base64
	status    func(txID string) string     // nil: every tx is CONFIRMED
	nonce     uint64
	keys      map[string][2]string // TSS key id -> {status, public key}
	rc        map[string]int64     // getAccountRC by account (absent: no record)
}

func newFakeL2() *fakeL2 {
	return &fakeL2{state: map[string]map[string]string{}, sigs: map[string]string{}, keys: map[string][2]string{}}
}

func (f *fakeL2) set(contract, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state[contract] == nil {
		f.state[contract] = map[string]string{}
	}
	f.state[contract][key] = value
}

func (f *fakeL2) gql(t *testing.T) *vscGraphQL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		var data any
		switch {
		case strings.Contains(req.Query, "getStateByKeys"):
			out := map[string]string{}
			st := f.state[fmt.Sprint(req.Variables["contractId"])]
			for _, k := range req.Variables["keys"].([]any) {
				if v, ok := st[k.(string)]; ok {
					out[k.(string)] = v
				}
			}
			data = map[string]any{"getStateByKeys": out}
		case strings.Contains(req.Query, "getTssRequests"):
			reqs := []map[string]string{}
			for _, m := range req.Variables["msgHex"].([]any) {
				if s, ok := f.sigs[m.(string)]; ok {
					reqs = append(reqs, map[string]string{"msg": m.(string), "sig": s, "status": "complete"})
				}
			}
			data = map[string]any{"getTssRequests": reqs}
		case strings.Contains(req.Query, "getTssKey"):
			var key any
			if k, ok := f.keys[fmt.Sprint(req.Variables["id"])]; ok {
				key = map[string]string{"status": k[0], "public_key": k[1]}
			}
			data = map[string]any{"getTssKey": key}
		case strings.Contains(req.Query, "getAccountRC"):
			var rec any
			if v, ok := f.rc[fmt.Sprint(req.Variables["a"])]; ok {
				rec = map[string]int64{"amount": v}
			}
			data = map[string]any{"getAccountRC": rec}
		case strings.Contains(req.Query, "getAccountNonce"):
			data = map[string]any{"getAccountNonce": map[string]any{"nonce": f.nonce}}
		case strings.Contains(req.Query, "submitTransactionV1"):
			b, _ := base64.URLEncoding.DecodeString(fmt.Sprint(req.Variables["tx"]))
			f.submitted = append(f.submitted, b)
			f.nonce++
			data = map[string]any{"submitTransactionV1": map[string]any{"id": fmt.Sprintf("l2tx-%d", len(f.submitted))}}
		case strings.Contains(req.Query, "findTransaction"):
			id := fmt.Sprint(req.Variables["filterOptions"].(map[string]any)["byId"])
			st := "CONFIRMED"
			if f.status != nil {
				st = f.status(id)
			}
			data = map[string]any{"findTransaction": []map[string]string{{"status": st}}}
		default:
			t.Errorf("fakeL2: unexpected query %.60s", req.Query)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return newVSCGraphQL([]string{srv.URL})
}

func writeFileForTest(path, content string) error { return os.WriteFile(path, []byte(content), 0600) }
