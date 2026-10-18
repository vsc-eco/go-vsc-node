package main

// Bot <-> bridge-contract wire tests. A fake Ethereum JSON-RPC serves a block
// built from real go-ethereum transactions and receipts, and every proof the
// bot builds is checked against go-ethereum's own trie roots, not the bot's.

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

const wireChainID = 11155111

type fakeChain struct {
	txs      []*types.Transaction
	receipts []*types.Receipt
}

// addTx signs tx with key and records a successful receipt carrying logs.
func (c *fakeChain) addTx(t *testing.T, key *ecdsa.PrivateKey, inner *types.DynamicFeeTx, logs ...*types.Log) *types.Transaction {
	t.Helper()
	inner.ChainID = big.NewInt(wireChainID)
	tx, err := types.SignTx(types.NewTx(inner), types.LatestSignerForChainID(big.NewInt(wireChainID)), key)
	if err != nil {
		t.Fatal(err)
	}
	cum := uint64(21000 * (len(c.receipts) + 1))
	for i, l := range logs {
		l.TxHash = tx.Hash()
		l.TxIndex = uint(len(c.txs))
		l.Index = uint(i)
	}
	rc := &types.Receipt{
		Type:              types.DynamicFeeTxType,
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: cum,
		Logs:              logs,
		TxHash:            tx.Hash(),
		TransactionIndex:  uint(len(c.txs)),
	}
	rc.Bloom = types.CreateBloom(types.Receipts{rc})
	c.txs = append(c.txs, tx)
	c.receipts = append(c.receipts, rc)
	return tx
}

func (c *fakeChain) txRoot() common.Hash {
	return types.DeriveSha(types.Transactions(c.txs), trie.NewStackTrie(nil))
}

func (c *fakeChain) receiptRoot() common.Hash {
	return types.DeriveSha(types.Receipts(c.receipts), trie.NewStackTrie(nil))
}

// rpc serves eth_getBlockByNumber / eth_getRawTransactionByHash /
// eth_getTransactionReceipt for the one block in c.
func (c *fakeChain) rpc(t *testing.T) *ethRPC {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad rpc request: %v", err)
			return
		}
		var hash string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &hash)
		}
		var result any
		switch req.Method {
		case "eth_getBlockByNumber":
			hashes := make([]string, len(c.txs))
			for i, tx := range c.txs {
				hashes[i] = tx.Hash().Hex()
			}
			result = map[string]any{"transactions": hashes}
		case "eth_getRawTransactionByHash":
			for _, tx := range c.txs {
				if tx.Hash().Hex() == hash {
					b, _ := tx.MarshalBinary()
					result = hexutil.Encode(b)
				}
			}
		case "eth_getTransactionReceipt":
			for _, rc := range c.receipts {
				if rc.TxHash.Hex() == hash {
					result = rc
				}
			}
		default:
			t.Errorf("unexpected rpc method %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(srv.Close)
	return newEthRPC(srv.URL)
}

// splitNodes splits concatenated RLP proof nodes (the bot's proof hex format).
func splitNodes(t *testing.T, proofHex string) [][]byte {
	t.Helper()
	b, err := hex.DecodeString(proofHex)
	if err != nil {
		t.Fatalf("proof hex: %v", err)
	}
	var nodes [][]byte
	for len(b) > 0 {
		_, _, rest, err := rlp.Split(b)
		if err != nil {
			t.Fatalf("proof node: %v", err)
		}
		nodes = append(nodes, b[:len(b)-len(rest)])
		b = rest
	}
	return nodes
}

// verifyLeaf checks that proofHex proves leafHex at index under root.
func verifyLeaf(t *testing.T, what string, root common.Hash, index int, leafHex, proofHex string) {
	t.Helper()
	db := memorydb.New()
	for _, n := range splitNodes(t, proofHex) {
		_ = db.Put(ethCrypto.Keccak256(n), n)
	}
	key, _ := rlp.EncodeToBytes(uint(index))
	got, err := trie.VerifyProof(root, key, db)
	if err != nil {
		t.Fatalf("%s: proof does not verify against go-ethereum root %s: %v", what, root, err)
	}
	if hex.EncodeToString(got) != leafHex {
		t.Fatalf("%s: proven leaf %x != raw_hex %s", what, got, leafHex)
	}
}

// contractPendingSpend is the bridge contract's stored PendingSpend
// (evm-mapping-contract contract/mapping/withdrawal.go), which the contract
// writes with encoding/json, so Amount lands as a bare JSON number.
type contractPendingSpend struct {
	Nonce         uint64   `json:"nonce"`
	Amount        *big.Int `json:"amount"`
	From          string   `json:"from"`
	To            string   `json:"to"`
	Asset         string   `json:"asset"`
	UnsignedTxHex string   `json:"unsigned_tx_hex"`
	BlockHeight   uint64   `json:"block_height"`
	VaultAtQueue  string   `json:"vault_at_queue"`
	TokenAddress  string   `json:"token_address,omitempty"`
}

// contractConfirmSpendRequest is the bridge contract's ConfirmSpendRequest
// (contract/mapping/types.go), decoded there with encoding/json.
type contractConfirmSpendRequest struct {
	BlockHeight     uint64 `json:"block_height"`
	TxIndex         uint64 `json:"tx_index"`
	TxHex           string `json:"tx_hex"`
	TxProofHex      string `json:"tx_proof_hex"`
	ReceiptHex      string `json:"receipt_hex"`
	ReceiptProofHex string `json:"receipt_proof_hex"`
	IntentNonce     uint64 `json:"intent_nonce"`
	IntentTo        string `json:"intent_to"`
	IntentAmount    string `json:"intent_amount"`
	IntentAsset     string `json:"intent_asset"`
}

// withdrawalFixture returns a block holding one vault withdrawal of amount wei
// and the contract-format PendingSpend JSON for it at nonce 0.
func withdrawalFixture(t *testing.T, amount *big.Int) (*fakeChain, string) {
	t.Helper()
	vaultKey, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	to := common.HexToAddress("0x00000000000000000000000000000000000000e2")
	c := &fakeChain{}
	c.addTx(t, vaultKey, &types.DynamicFeeTx{
		Nonce: 0, GasTipCap: big.NewInt(2e9), GasFeeCap: big.NewInt(40e9), Gas: 21000, To: &to, Value: amount,
	})
	raw, err := json.Marshal(contractPendingSpend{
		Nonce: 0, Amount: amount, From: "hive:alice", To: to.Hex(), Asset: "eth",
		UnsignedTxHex: "02ee", BlockHeight: 100, VaultAtQueue: vault.Hex(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, string(raw)
}

func checkConfirmSpendWire(t *testing.T, amount *big.Int) {
	t.Helper()
	c, stored := withdrawalFixture(t, amount)
	ps := parsePendingSpend(0, stored)
	if ps == nil {
		t.Fatalf("bot refused the contract's PendingSpend: %s", stored)
	}
	payload, err := buildConfirmSpendPayload(c.rpc(t), c.txs[0].Hash().Hex(), 101, 0, ps)
	if err != nil {
		t.Fatal(err)
	}
	var req contractConfirmSpendRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("the contract cannot decode the bot's confirmSpend: %v\npayload: %s", err, payload)
	}
	if req.IntentAmount != amount.String() {
		t.Fatalf("intent_amount = %q, want %q", req.IntentAmount, amount.String())
	}
	verifyLeaf(t, "tx leg", c.txRoot(), 0, req.TxHex, req.TxProofHex)
	verifyLeaf(t, "receipt leg", c.receiptRoot(), 0, req.ReceiptHex, req.ReceiptProofHex)
}

// E2: the contract decodes intent_amount into a string (decimal wei) and
// encoding/json refuses a JSON number for it, so a numeric intent_amount
// made every bot confirmSpend fail at decode.
func TestE2_ConfirmSpendIntentAmountIsDecimalString(t *testing.T) {
	checkConfirmSpendWire(t, big.NewInt(1_000_000_000_000_000_000)) // 1 ETH
}

// E3: the contract stores PendingSpend.Amount as a big.Int, written as a
// bare JSON number. A withdrawal of 2^63 wei (about 9.22 ETH, or 9.22 of any
// 18-decimal token) or more did not fit the bot's int64, so the bot refused
// the entry and the withdrawal was never broadcast.
func TestE3_WithdrawalAboveInt64IsCarriedExactly(t *testing.T) {
	for _, s := range []string{
		"9223372036854775808",        // 2^63 wei
		"10000000000000000000",       // 10 ETH
		"99999999999999999999999999", // 1e26 wei
	} {
		amt, _ := new(big.Int).SetString(s, 10)
		_, stored := withdrawalFixture(t, amt)
		ps := parsePendingSpend(0, stored)
		if ps == nil {
			t.Fatalf("%s wei: bot refused the contract's PendingSpend", s)
		}
		if got := fmt.Sprint(ps.Amount); got != s {
			t.Fatalf("%s wei: parsed amount %s", s, got)
		}
		checkConfirmSpendWire(t, amt)
	}
}

// A zero, negative or missing amount in the contract's JSON format is still
// refused. (The older negative/empty tests use the pipe format, which fails
// to decode for an unrelated reason.)
func TestE3_ParsePendingSpend_RefusesNonPositiveAmount(t *testing.T) {
	for _, amount := range []string{`0`, `-1`, `null`} {
		raw := `{"nonce":0,"amount":` + amount + `,"from":"hive:alice","to":"0x00000000000000000000000000000000000000e2","asset":"eth","unsigned_tx_hex":"02ee","block_height":100,"vault_at_queue":"0x1"}`
		if ps := parsePendingSpend(0, raw); ps != nil {
			t.Fatalf("amount %s accepted: %+v", amount, ps)
		}
	}
	raw := `{"nonce":0,"from":"hive:alice","to":"0x00000000000000000000000000000000000000e2","asset":"eth","unsigned_tx_hex":"02ee","block_height":100}`
	if ps := parsePendingSpend(0, raw); ps != nil {
		t.Fatalf("missing amount accepted: %+v", ps)
	}
}
