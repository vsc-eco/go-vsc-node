//go:build evm_realwasm

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/btcvault"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// rwDeepTrie is a go-ethereum trie holding target (value val) with a full
// branch at each of its key's first depth nibbles (15 neighbours a level),
// so target's proof is as deep and as large as a mainnet one.
func rwDeepTrie(target, val []byte, depth int) *trie.Trie {
	tr := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	if val != nil {
		tr.MustUpdate(target, val)
	}
	n := int64(1)
	for i := 0; i < depth; i++ {
		for v := byte(0); v < 16; v++ {
			k := append([]byte{}, target...)
			b := k[i/2]
			if i%2 == 0 {
				b = v<<4 | b&0x0f
			} else {
				b = b&0xf0 | v
			}
			if b == k[i/2] {
				continue
			}
			k[i/2] = b
			copy(k[i/2+1:], ethCrypto.Keccak256([]byte{byte(i), v}))
			enc, _ := gethrlp.EncodeToBytes(big.NewInt(n).Bytes())
			tr.MustUpdate(k, enc)
			n++
		}
	}
	return tr
}

func rwProveHex(t *testing.T, tr *trie.Trie, key []byte) []string {
	t.Helper()
	var p proofList
	if err := tr.Prove(key, &p); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(p))
	for i, n := range p {
		out[i] = hexutil.Encode(n)
	}
	return out
}

// rwTokenState is a mainnet-depth state holding token, whose storage holds
// word at holder's balance key (slot) among neighbours, served by an
// eth_getProof RPC that proves any storage key asked. It returns the RPC and
// the state root.
func rwTokenState(t *testing.T, token, holder common.Address, slot uint64, word *big.Int) (*ethRPC, common.Hash) {
	t.Helper()
	slotKey := common.HexToHash(tokenSlotKey(holder.Hex(), slot))
	var val []byte
	if word.Sign() > 0 {
		val, _ = gethrlp.EncodeToBytes(word.Bytes())
	}
	storage := rwDeepTrie(ethCrypto.Keccak256(slotKey.Bytes()), val, 6)
	sroot := storage.Hash()
	acct, _ := gethrlp.EncodeToBytes(&types.StateAccount{Nonce: 1, Balance: uint256.NewInt(0), Root: sroot, CodeHash: ethCrypto.Keccak256([]byte("FiatTokenProxy"))})
	state := rwDeepTrie(ethCrypto.Keccak256(token.Bytes()), acct, 8)
	root := state.Hash()
	accountProof := rwProveHex(t, state, ethCrypto.Keccak256(token.Bytes()))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var keys []string
		if len(req.Params) > 1 {
			_ = json.Unmarshal(req.Params[1], &keys)
		}
		sps := []map[string]any{}
		for _, k := range keys {
			sps = append(sps, map[string]any{"key": k, "value": "0x0",
				"proof": rwProveHex(t, storage, ethCrypto.Keccak256(common.HexToHash(k).Bytes()))})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"accountProof": accountProof, "storageHash": sroot.Hex(), "storageProof": sps}})
	}))
	t.Cleanup(srv.Close)
	return newEthRPC(srv.URL), root
}

// C2 for tokens on the bridge wasm, with the bot's own setting parse,
// eth_getProof fetch and call payloads: USDC held, short (its flag and "si"),
// ETH's held proof leaves USDC's halt, the blacklist bit masked, held again
// lifts it; a header older than the bridge's deposits is refused.
func TestRealWasm_TC_UsdcCustodyProof(t *testing.T) {
	c, _ := rwUSDCBridge(t)
	if msg, ok := rwOwnerAction(t, c, "setTokenCustody", `{"asset":"usdc","slot":9,"bits":255,"gap":"0"}`); !ok {
		t.Fatalf("setTokenCustody: %s", msg)
	}
	toks := parseTokenCustody(map[string]string{"tcc-usdc": c.ct.StateGet(rwBridge, "tcc-usdc"), "ts-usdc": c.ct.StateGet(rwBridge, "ts-usdc")}, []string{"usdc"})
	if len(toks) != 1 || toks[0].Slot != 9 || !strings.EqualFold(toks[0].Address, rwUSDC) {
		t.Fatalf("the bot read the setting as %+v (tcc %q, ts %q)", toks, c.ct.StateGet(rwBridge, "tcc-usdc"), c.ct.StateGet(rwBridge, "ts-usdc"))
	}
	tk := toks[0]
	book, _ := new(big.Int).SetString(strings.Split(c.ct.StateGet(rwBridge, "s-usdc"), "|")[0], 10)
	if book == nil || book.Sign() <= 0 {
		t.Fatalf("booked usdc custody %q", c.ct.StateGet(rwBridge, "s-usdc"))
	}
	gens := []vaultGen{{Gen: 0, Status: btcvault.VaultStatusActive, Address: strings.ToLower(c.vault.Hex())}}
	round := func(height uint64, word *big.Int) (string, bool, []int64, []int) {
		t.Helper()
		rpc, root := rwTokenState(t, common.HexToAddress(tk.Address), c.vault, tk.Slot, word)
		rwHeaderState(c.ct, height, root, &fakeChain{}, rwBlockTime)
		acct, sps, err := fetchStorageProofs(context.Background(), rpc, tk.Address, []string{tokenSlotKey(gens[0].Address, tk.Slot)}, height)
		if err != nil {
			t.Fatal(err)
		}
		var rcs []int64
		var sizes []int
		var last string
		for _, call := range tokenCustodyCalls(tk, gens, height, acct, sps) {
			r := rwCallLimit(c.ct, "hive:anyone", call.Action, call.Payload, 50_000)
			if !r.Success {
				return call.Action + ": " + r.ErrMsg, false, rcs, sizes
			}
			rcs, sizes, last = append(rcs, r.RcUsed), append(sizes, len(call.Payload)), r.Ret
		}
		return last, true, rcs, sizes
	}
	flags := func() (string, string) { return c.ct.StateGet(rwBridge, "si"), c.ct.StateGet(rwBridge, "sis-usdc") }

	out, ok, rcs, sizes := round(110, book)
	if si, f := flags(); !ok || !strings.Contains(out, "custody held") || si != "" || f != "" {
		t.Fatalf("usdc held: %q ok=%v si=%q sis-usdc=%q", out, ok, si, f)
	}
	t.Logf("proveTokenRoot %d RC (payload %d bytes), proveTokenCustody %d RC (payload %d bytes)", rcs[0], sizes[0], rcs[1], sizes[1])

	short := new(big.Int).Sub(book, big.NewInt(1))
	if out, ok, _, _ := round(111, short); !ok || !strings.Contains(out, "custody short") {
		t.Fatalf("usdc short by 1: %q ok=%v", out, ok)
	}
	if si, f := flags(); si == "" || f == "" {
		t.Fatalf("usdc short: si=%q sis-usdc=%q", si, f)
	}

	// ETH held: the halt stays (USDC is short).
	ethBook, _ := new(big.Int).SetString(strings.Split(c.ct.StateGet(rwBridge, "s-eth"), "|")[0], 10)
	proofRPC, root := rwStateProof(t, c.vault, 0, ethBook)
	rwHeaderState(c.ct, 112, root, &fakeChain{}, rwBlockTime)
	ep, _, _, err := fetchAccountProof(context.Background(), proofRPC, c.vault.Hex(), 112)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(map[string]any{"gen": 0, "proof": ep})
	if r := rwCallLimit(c.ct, "hive:anyone", "proveCustody", p, 50_000); !r.Success || !strings.Contains(r.Ret, "custody held") {
		t.Fatalf("eth held: %s %s", r.Ret, r.ErrMsg)
	}
	if si, f := flags(); si == "" || f == "" {
		t.Fatalf("ETH's held proof lifted USDC's halt: si=%q sis-usdc=%q", si, f)
	}

	// The blacklist flag (bit 255) with the whole book: held, lifted.
	flagged := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 255), book)
	if out, ok, _, _ := round(113, flagged); !ok || !strings.Contains(out, "custody held") {
		t.Fatalf("flagged word with the book: %q ok=%v", out, ok)
	}
	if si, f := flags(); si != "" || f != "" {
		t.Fatalf("held again: si=%q sis-usdc=%q (deleted, not overwritten)", si, f)
	}

	if out, ok, _, _ := round(50, book); ok || !strings.Contains(out, "credited deposit") {
		t.Fatalf("a header older than the bridge's deposits: %q ok=%v", out, ok)
	}
}

// The root step on real data: mainnet USDC's account proof at block
// 26,330,745 through the bridge wasm gives the storage root the RPC reported.
func TestRealWasm_TC_MainnetUsdcRoot(t *testing.T) {
	raw, err := os.ReadFile("testdata/usdc_mainnet_storage_proof.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Block        uint64   `json:"block"`
		StateRoot    string   `json:"stateRoot"`
		Token        string   `json:"token"`
		StorageHash  string   `json:"storageHash"`
		AccountProof []string `json:"accountProof"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	c, _ := rwUSDCBridge(t)
	if msg, ok := rwRegisterToken(t, c, fx.Token, "musdc"); !ok {
		t.Fatalf("register mainnet usdc: %s", msg)
	}
	if msg, ok := rwOwnerAction(t, c, "setTokenCustody", `{"asset":"musdc","slot":9,"bits":255,"gap":"0"}`); !ok {
		t.Fatalf("setTokenCustody: %s", msg)
	}
	rwHeaderState(c.ct, fx.Block, common.HexToHash(fx.StateRoot), &fakeChain{}, rwBlockTime)
	var nodes []byte
	for _, n := range fx.AccountProof {
		nodes = append(nodes, hexToBytes(n)...)
	}
	acct := accountProof{BlockHeight: fx.Block, Proof: base64.StdEncoding.EncodeToString(nodes)}
	calls := tokenCustodyCalls(tokenCustody{Symbol: "musdc", Address: strings.ToLower(fx.Token), Slot: 9}, nil, fx.Block, acct, nil)
	r := rwCallLimit(c.ct, "hive:anyone", calls[0].Action, calls[0].Payload, 50_000)
	if !r.Success {
		t.Fatalf("proveTokenRoot (mainnet): %s %s", r.Err, r.ErrMsg)
	}
	want := strings.TrimPrefix(strings.ToLower(fx.StorageHash), "0x")
	if got := c.ct.StateGet(rwBridge, "tcp-musdc"); !strings.Contains(got, "|"+want+"|") {
		t.Fatalf("stored root %q, mainnet storageHash %s", got, want)
	}
	t.Logf("proveTokenRoot on mainnet USDC: %d RC, payload %d bytes", r.RcUsed, len(calls[0].Payload))
}
