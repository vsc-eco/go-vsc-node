//go:build evm_realwasm

package main

// E5 on the node's wasm runtime: the compiled zk-header-verifier backfills an
// open header gap with submitHeaders, from post-merge header RLP as geth
// encodes it, closes the gap where it meets the stored chain, and refuses a
// proof while it is open. The gap is planted as submitProof leaves it (the
// proof's accept path needs a real SP1 proof).
//
//	ZK_VERIFIER_WASM=/path/to/zk-header-verifier/bin/main.wasm go test -tags evm_realwasm ./cmd/evm-mapping-bot/ -run RealWasm_ZK -v

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
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

func TestRealWasm_ZK_BackfillClosesTheHeaderGap(t *testing.T) {
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
	// A 12-header backfill costs ~32k RC (state writes, 19 RC a byte), more
	// than a Hive account's free 10k.
	ct.Deposit(rwRelayer, 1_000_000, ledgerDb.AssetHbd)

	// Stored chain up to 100; a proof stored 129..140 and opened the gap.
	chain := rwZkChain(100, 41, common.Hash{0xa0}) // 100..140
	hex32 := func(h common.Hash) string { return hex.EncodeToString(h[:]) }
	for k, v := range map[string]string{
		"h": "140", "lbh": "0x" + hex32(chain[40].Hash()), "cid": "11155111", "mr": "50400",
		"gapw": hex32(chain[28].Hash()), "gapl": "129", "gapb": "100", "gapa": hex32(chain[0].Hash()),
	} {
		ct.StateSet(rwZk, k, v)
	}

	if r := rwZkCall(ct, "submitProof", []byte(`{"proof":"00","public_values":"`+strings.Repeat("0", 768)+`","headers":[{"rlp_hex":"00"}]}`)); r.Success || !strings.Contains(r.ErrMsg, "gap is open") {
		t.Fatalf("submitProof with the gap open: success=%v %s", r.Success, r.ErrMsg)
	}
	if r := rwZkCall(ct, "submitHeaders", rwZkBatch(t, rwZkChain(117, 12, common.Hash{0xbb}))); r.Success || !strings.Contains(r.ErrMsg, "not the parent") {
		t.Fatalf("backfill off the proven chain: success=%v %s", r.Success, r.ErrMsg)
	}
	for _, b := range [][2]int{{17, 29}, {5, 17}, {1, 5}} { // 117..128, 105..116, 101..104
		r := rwZkCall(ct, "submitHeaders", rwZkBatch(t, chain[b[0]:b[1]]))
		if !r.Success {
			t.Fatalf("backfill %d..%d: %s: %s", 100+b[0], 99+b[1], r.Err, r.ErrMsg)
		}
		// Measured 2,640 RC a header (the 137-byte header write); the same per
		// header as submitProof's own header writes.
		if per := r.RcUsed / int64(b[1]-b[0]); per > 3000 {
			t.Errorf("backfill costs %d RC a header, measured 2,640", per)
		}
	}
	for _, k := range []string{"gapw", "gapl", "gapb", "gapa"} {
		if v := ct.StateGet(rwZk, k); v != "" {
			t.Fatalf("gap key %s = %q after the backfill met the stored chain", k, v)
		}
	}
	// The v1 encoding account-mapping reads (blocklist/blocks.go).
	for _, h := range chain[1:29] {
		n := h.Number.Uint64()
		want := []byte{0x01}
		want = binary.BigEndian.AppendUint64(want, n)
		want = append(append(append(want, h.Root[:]...), h.TxHash[:]...), h.ReceiptHash[:]...)
		for _, v := range []uint64{7, 36_000_000, h.Time, 11155111} {
			want = binary.BigEndian.AppendUint64(want, v)
		}
		if got := ct.StateGet(rwZk, "b-"+strconv.FormatUint(n, 10)); got != string(want) {
			t.Fatalf("b-%d = %x, want %x", n, got, want)
		}
	}
	if r := rwZkCall(ct, "submitHeaders", rwZkBatch(t, chain[1:5])); r.Success || !strings.Contains(r.ErrMsg, "no header gap is open") {
		t.Fatalf("submitHeaders after the gap closed: success=%v %s", r.Success, r.ErrMsg)
	}
}
