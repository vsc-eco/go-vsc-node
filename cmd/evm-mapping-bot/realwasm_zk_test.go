//go:build evm_realwasm

package main

// RP-9 on the node's wasm runtime: the compiled zk-header-verifier stores the
// headers of blocks below a proven one from real EIP-2935 proofs (mainnet and
// Sepolia, fetched 2026-10-27, contract/testdata), and links headers down from
// a stored one by parent hash, from post-merge header RLP as geth encodes it.
// The proven root is planted as submitProof stores it (the proof's accept path
// needs a real SP1 proof).
//
//	ZK_VERIFIER_WASM=/path/to/zk-header-verifier/bin/main.wasm go test -tags evm_realwasm ./cmd/evm-mapping-bot/ -run RealWasm_ZK -v

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

const rwZk = "vsc1RWzkverifier"

// rwZkChain is n Prague-shaped headers from `from` on top of parent.
func rwZkChain(from uint64, n int, parent common.Hash) []*types.Header {
	var out []*types.Header
	zero, blob := common.Hash{}, uint64(0)
	for i := 0; i < n; i++ {
		num := from + uint64(i)
		h := &types.Header{
			ParentHash: parent, UncleHash: types.EmptyUncleHash, Root: common.Hash{0x5e, byte(num)},
			TxHash: common.Hash{0x7e, byte(num)}, ReceiptHash: common.Hash{0x8e, byte(num)},
			Difficulty: new(big.Int), Number: new(big.Int).SetUint64(num), GasLimit: 36_000_000, GasUsed: 21_000,
			Time: 1_760_000_000 + num*12, Extra: []byte("rw"), BaseFee: big.NewInt(7),
			WithdrawalsHash: &types.EmptyWithdrawalsHash, BlobGasUsed: &blob, ExcessBlobGas: &blob,
			ParentBeaconRoot: &zero, RequestsHash: &zero,
		}
		out = append(out, h)
		parent = h.Hash()
	}
	return out
}

func rwZkBatch(t *testing.T, hs []*types.Header) []byte {
	t.Helper()
	var items []map[string]string
	for _, h := range hs {
		b, err := gethrlp.EncodeToBytes(h)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, map[string]string{"rlp_hex": hex.EncodeToString(b)})
	}
	p, _ := json.Marshal(map[string]any{"headers": items})
	return p
}

func rwZkCall(ct *test_utils.ContractTest, action string, payload []byte) test_utils.ContractTestCallResult {
	return ct.Call(stateEngine.TxVscCallContract{
		Self: stateEngine.TxSelf{
			TxId: "rwzk", BlockId: "block:rw", Index: 1, Timestamp: "2026-10-21T00:00:00",
			RequiredAuths: []string{rwRelayer}, RequiredPostingAuths: []string{},
		},
		ContractId: rwZk, Action: action, Payload: payload,
		RcLimit: 100_000, Intents: []contracts.Intent{}, Caller: rwRelayer,
	})
}

// rwZkStored is header h as the verifier stores it (the v1 encoding
// account-mapping reads, blocklist/blocks.go), labelled Sepolia.
func rwZkStored(h *types.Header) string {
	b := []byte{0x01}
	b = binary.BigEndian.AppendUint64(b, h.Number.Uint64())
	b = append(append(append(b, h.Root[:]...), h.TxHash[:]...), h.ReceiptHash[:]...)
	for _, v := range []uint64{h.BaseFee.Uint64(), h.GasLimit, h.Time, 11155111} {
		b = binary.BigEndian.AppendUint64(b, v)
	}
	return string(b)
}

// rwZkVerifier deploys the compiled verifier with the stored tip at h.
func rwZkVerifier(t *testing.T, tip uint64) (*test_utils.ContractTest, string) {
	t.Helper()
	path := os.Getenv("ZK_VERIFIER_WASM")
	if path == "" {
		t.Skip("ZK_VERIFIER_WASM not set")
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := test_utils.NewContractTest()
	ct := &c
	ct.RegisterContract(rwZk, "hive:owner", wasm)
	ct.Deposit(rwRelayer, 1_000_000, ledgerDb.AssetHbd)
	for k, v := range map[string]string{"h": strconv.FormatUint(tip, 10), "cid": "11155111", "mr": "648000"} {
		ct.StateSet(rwZk, k, v)
	}
	return ct, filepath.Join(filepath.Dir(path), "..", "contract", "testdata")
}

func TestRealWasm_ZK_RP9_AttestHeadersFromRealEIP2935Proofs(t *testing.T) {
	for _, network := range []string{"mainnet", "sepolia"} {
		t.Run(network, func(t *testing.T) {
			var f struct {
				RootBlock    uint64   `json:"root_block"`
				RootRLP      string   `json:"root_rlp"`
				StorageHash  string   `json:"storage_hash"`
				AccountProof []string `json:"account_proof"`
				Headers      []struct {
					Number       uint64   `json:"number"`
					RLP          string   `json:"rlp"`
					StorageProof []string `json:"storage_proof"`
				} `json:"headers"`
			}
			ct, dir := rwZkVerifier(t, 0)
			raw, err := os.ReadFile(filepath.Join(dir, "eip2935_"+network+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			nodes := func(ns []string) string {
				var all []byte
				for _, n := range ns {
					all = append(all, common.FromHex(n)...)
				}
				return base64.StdEncoding.EncodeToString(all)
			}
			// The fields the verifier stores, read by position: geth v1.14 does
			// not know the Glamsterdam fields Sepolia headers carry (block access
			// list hash, slot number), so types.Header cannot decode them.
			decode := func(rlpHex string) *types.Header {
				body, _, err := gethrlp.SplitList(common.FromHex(rlpHex))
				if err != nil {
					t.Fatal(err)
				}
				var f [][]byte
				for len(body) > 0 && len(f) < 16 {
					var v []byte
					if _, v, body, err = gethrlp.Split(body); err != nil {
						t.Fatal(err)
					}
					f = append(f, v)
				}
				u := func(b []byte) uint64 { return new(big.Int).SetBytes(b).Uint64() }
				return &types.Header{Root: common.BytesToHash(f[3]), TxHash: common.BytesToHash(f[4]), ReceiptHash: common.BytesToHash(f[5]),
					Number: new(big.Int).SetBytes(f[8]), GasLimit: u(f[9]), Time: u(f[11]), BaseFee: new(big.Int).SetBytes(f[15])}
			}
			// Plant root block R as submitProof stores it.
			r := f.RootBlock
			ct.StateSet(rwZk, "h", strconv.FormatUint(r, 10))
			ct.StateSet(rwZk, "b-"+strconv.FormatUint(r, 10), rwZkStored(decode(f.RootRLP)))

			p, _ := json.Marshal(map[string]any{"block_number": r, "account_proof": nodes(f.AccountProof)})
			if len(p) > 8192 {
				t.Fatalf("attestHistoryRoot payload %d bytes > the 8,192-byte cap", len(p))
			}
			res := rwZkCall(ct, "attestHistoryRoot", p)
			if !res.Success {
				t.Fatalf("attestHistoryRoot: %s: %s", res.Err, res.ErrMsg)
			}
			if got := hex.EncodeToString([]byte(ct.StateGet(rwZk, "hr-"+strconv.FormatUint(r, 10)))); got != strings.TrimPrefix(f.StorageHash, "0x") {
				t.Fatalf("hr-%d = %s, want %s", r, got, f.StorageHash)
			}
			t.Logf("attestHistoryRoot: payload %d B, %d RC", len(p), res.RcUsed)

			var items []map[string]string
			for _, h := range f.Headers {
				items = append(items, map[string]string{"rlp_hex": strings.TrimPrefix(h.RLP, "0x"), "storage_proof": nodes(h.StorageProof)})
			}
			one, _ := json.Marshal(map[string]any{"root_block": r, "headers": items[:1]})
			two, _ := json.Marshal(map[string]any{"root_block": r, "headers": items[1:3]})
			if len(one) > 8192 || len(two) > 8192 {
				t.Fatalf("attestHeaders payloads %d (1 header), %d (2 headers) bytes; cap 8,192", len(one), len(two))
			}
			r1 := rwZkCall(ct, "attestHeaders", one)
			r2 := rwZkCall(ct, "attestHeaders", two)
			if !r1.Success || !r2.Success {
				t.Fatalf("attestHeaders: %s %s / %s %s", r1.Err, r1.ErrMsg, r2.Err, r2.ErrMsg)
			}
			t.Logf("attestHeaders: 1 header %d B %d RC; 2 headers %d B %d RC", len(one), r1.RcUsed, len(two), r2.RcUsed)
			for _, h := range f.Headers {
				if got := ct.StateGet(rwZk, "b-"+strconv.FormatUint(h.Number, 10)); got != rwZkStored(decode(h.RLP)) {
					t.Fatalf("b-%d = %x, want the attested header", h.Number, got)
				}
			}
			if got := ct.StateGet(rwZk, "h"); got != strconv.FormatUint(r, 10) {
				t.Fatalf("attesting moved h to %s", got)
			}
			// A header the proof does not show (another block's proof) is refused.
			items[0]["storage_proof"] = items[1]["storage_proof"]
			bad, _ := json.Marshal(map[string]any{"root_block": r, "headers": items[:1]})
			if res := rwZkCall(ct, "attestHeaders", bad); res.Success {
				t.Fatal("a header attested with another block's storage proof")
			}
		})
	}
}

// The fallback: headers linked by parent hash down from a stored header; one
// off the chain is refused. Measured: what one linked header costs.
func TestRealWasm_ZK_RP9_SubmitHeadersLinksDown(t *testing.T) {
	ct, _ := rwZkVerifier(t, 140)
	chain := rwZkChain(100, 41, common.Hash{0xa0}) // 100..140
	// Block 140 stored as a proof stores it: its header and its parent hash.
	ct.StateSet(rwZk, "b-140", rwZkStored(chain[40]))
	ct.StateSet(rwZk, "ph-140", string(chain[39].Hash().Bytes()))
	if r := rwZkCall(ct, "submitHeaders", rwZkBatch(t, rwZkChain(134, 6, common.Hash{0xbb}))); r.Success || !strings.Contains(r.ErrMsg, "not the stored parent") {
		t.Fatalf("headers off the stored chain: success=%v %s", r.Success, r.ErrMsg)
	}
	for _, b := range [][2]int{{34, 40}, {28, 34}} { // 134..139, 128..133
		r := rwZkCall(ct, "submitHeaders", rwZkBatch(t, chain[b[0]:b[1]]))
		if !r.Success {
			t.Fatalf("link %d..%d: %s: %s", 100+b[0], 99+b[1], r.Err, r.ErrMsg)
		}
		t.Logf("submitHeaders %d headers: %d RC (%d a header)", b[1]-b[0], r.RcUsed, r.RcUsed/int64(b[1]-b[0]))
	}
	for _, h := range chain[28:40] {
		n := h.Number.Uint64()
		if got := ct.StateGet(rwZk, "b-"+strconv.FormatUint(n, 10)); got != rwZkStored(h) {
			t.Fatalf("b-%d = %x, want the linked header", n, got)
		}
	}
	if got := ct.StateGet(rwZk, "h"); got != "140" {
		t.Fatalf("submitHeaders moved h to %s", got)
	}
}
