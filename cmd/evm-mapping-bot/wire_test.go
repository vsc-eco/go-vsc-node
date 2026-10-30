package main

// Bot <-> bridge-contract wire tests. A fake Ethereum JSON-RPC serves a block
// built from real go-ethereum transactions and receipts, and every proof the
// bot builds is checked against go-ethereum's own trie roots, not the bot's.

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
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
	prior := 0 // logs in earlier receipts: RPC log indexes are block-level
	for _, rc := range c.receipts {
		prior += len(rc.Logs)
	}
	for i, l := range logs {
		l.TxHash = tx.Hash()
		l.TxIndex = uint(len(c.txs))
		l.Index = uint(prior + i)
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
			var full bool
			if len(req.Params) > 1 {
				_ = json.Unmarshal(req.Params[1], &full)
			}
			if !full {
				hashes := make([]string, len(c.txs))
				for i, tx := range c.txs {
					hashes[i] = tx.Hash().Hex()
				}
				result = map[string]any{"transactions": hashes}
				break
			}
			txs := make([]map[string]string, len(c.txs))
			for i, tx := range c.txs {
				from, _ := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
				txs[i] = map[string]string{"hash": tx.Hash().Hex(), "from": from.Hex(), "to": tx.To().Hex(), "value": hexutil.EncodeBig(tx.Value())}
			}
			result = map[string]any{"number": "0x65", "transactions": txs}
		case "eth_getLogs":
			// block-level log indexes across the block, filtered by address
			var f struct {
				Address string    `json:"address"`
				Topics  []*string `json:"topics"`
			}
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params[0], &f)
			}
			logs := []map[string]any{}
			match := func(l *types.Log) bool {
				if f.Address != "" && !strings.EqualFold(f.Address, l.Address.Hex()) {
					return false
				}
				for i, want := range f.Topics {
					if want != nil && (i >= len(l.Topics) || !strings.EqualFold(*want, l.Topics[i].Hex())) {
						return false
					}
				}
				return true
			}
			bi := 0
			for _, rc := range c.receipts {
				for _, l := range rc.Logs {
					if match(l) {
						topics := make([]string, len(l.Topics))
						for i, tp := range l.Topics {
							topics[i] = tp.Hex()
						}
						logs = append(logs, map[string]any{"address": l.Address.Hex(), "topics": topics, "data": hexutil.Encode(l.Data),
							"logIndex": hexutil.EncodeUint64(uint64(bi)), "transactionIndex": hexutil.EncodeUint64(uint64(l.TxIndex)), "transactionHash": l.TxHash.Hex()})
					}
					bi++
				}
			}
			result = logs
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
	BlockHeight  uint64 `json:"block_height"`
	TxIndex      uint64 `json:"tx_index"`
	TxProof      string `json:"tx_proof"`
	ReceiptProof string `json:"receipt_proof"`
	IntentNonce  uint64 `json:"intent_nonce"`
	IntentTo     string `json:"intent_to"`
	IntentAmount string `json:"intent_amount"`
	IntentAsset  string `json:"intent_asset"`
}

// verifyLeafB64 checks that the base64 proof proves leaf at index under root:
// the contract takes the tx and receipt from these proven leaves.
func verifyLeafB64(t *testing.T, what string, root common.Hash, index int, leaf []byte, proofB64 string) {
	t.Helper()
	proof, err := base64.StdEncoding.DecodeString(proofB64)
	if err != nil {
		t.Fatalf("%s: proof is not base64: %v", what, err)
	}
	verifyLeaf(t, what, root, index, hex.EncodeToString(leaf), hex.EncodeToString(proof))
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
	payload, err := buildConfirmSpendPayload(context.Background(), c.rpc(t), c.txs[0].Hash().Hex(), 101, 0, ps)
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
	rawTx, _ := c.txs[0].MarshalBinary()
	rawReceipt, _ := c.receipts[0].MarshalBinary()
	verifyLeafB64(t, "tx leg", c.txRoot(), 0, rawTx, req.TxProof)
	verifyLeafB64(t, "receipt leg", c.receiptRoot(), 0, rawReceipt, req.ReceiptProof)
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

// erc20DepositFixture returns a block whose tx 1 is an ERC-20 transfer of
// amount to vault (tx 0 is an unrelated ETH transfer), plus the deposit the
// scanner would report for it.
func erc20DepositFixture(t *testing.T, vault common.Address, amount *big.Int) (*fakeChain, detectedDeposit) {
	t.Helper()
	userKey, _ := ethCrypto.GenerateKey()
	user := ethCrypto.PubkeyToAddress(userKey.PublicKey)
	token := common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238")
	other := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	c := &fakeChain{}
	c.addTx(t, userKey, &types.DynamicFeeTx{
		Nonce: 0, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &other, Value: big.NewInt(1),
	})
	transferSig := ethCrypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	tx := c.addTx(t, userKey, &types.DynamicFeeTx{
		Nonce: 1, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 60000, To: &token,
		Data: append(append(common.FromHex("a9059cbb"), common.LeftPadBytes(vault.Bytes(), 32)...), common.LeftPadBytes(amount.Bytes(), 32)...),
	}, &types.Log{
		Address: token,
		Topics:  []common.Hash{transferSig, common.BytesToHash(user.Bytes()), common.BytesToHash(vault.Bytes())},
		Data:    common.LeftPadBytes(amount.Bytes(), 32),
	})
	return c, detectedDeposit{
		BlockHeight: 101, TxIndex: 1, LogIndex: 0, TxHash: tx.Hash().Hex(),
		DepositType: "erc20", TokenAddress: token.Hex(),
	}
}

type contractMapParams struct {
	TxData struct {
		BlockHeight    uint64 `json:"block_height"`
		TxIndex        uint64 `json:"tx_index"`
		RawHex         string `json:"raw_hex"`
		MerkleProofHex string `json:"merkle_proof_hex"`
		LogIndex       uint64 `json:"log_index"`
		TokenAddress   string `json:"token_address"`
		DepositType    string `json:"deposit_type"`
		ReceiptProof   string `json:"receipt_proof"`
	} `json:"tx_data"`
	Instructions []string `json:"instructions"`
}

// E4: the contract verifies an ERC-20 deposit as a RECEIPT against
// header.ReceiptsRoot (VerifyERC20Deposit) and reads the Transfer log from
// it. The bot always sent the transaction and a transactions-trie proof, so
// every ERC-20 deposit failed the proof check.
func TestE4_ERC20MapPayloadIsReceiptProof(t *testing.T) {
	vault := common.HexToAddress("0x00000000000000000000000000000000000000fa")
	c, dep := erc20DepositFixture(t, vault, big.NewInt(25_000_000)) // 25 USDC
	payload := buildMapPayloadFromRPC(context.Background(), c.rpc(t), dep, dep.BlockHeight)
	if payload == nil {
		t.Fatal("no map payload")
	}
	var p contractMapParams
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("the contract cannot decode the bot's map payload: %v", err)
	}
	// ERC20-PAYLOAD-1: base64, the receipt taken from the proven leaf, not
	// sent again as raw_hex.
	want, _ := c.receipts[dep.TxIndex].MarshalBinary()
	if p.TxData.RawHex != "" || p.TxData.MerkleProofHex != "" || p.TxData.ReceiptProof == "" {
		t.Fatalf("erc20 map carries raw_hex=%d chars, merkle_proof_hex=%d chars, receipt_proof=%d chars; want only receipt_proof",
			len(p.TxData.RawHex), len(p.TxData.MerkleProofHex), len(p.TxData.ReceiptProof))
	}
	verifyLeafB64(t, "erc20 receipt", c.receiptRoot(), dep.TxIndex, want, p.TxData.ReceiptProof)
	if p.TxData.DepositType != "erc20" || p.TxData.LogIndex != 0 || p.TxData.TokenAddress != dep.TokenAddress {
		t.Fatalf("erc20 fields: %+v", p.TxData)
	}
}

// Native ETH deposits stay transaction proofs (VerifyETHDeposit checks
// header.TransactionsRoot).
func TestE4_ETHMapPayloadIsTransactionProof(t *testing.T) {
	vaultKey, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	userKey, _ := ethCrypto.GenerateKey()
	c := &fakeChain{}
	tx := c.addTx(t, userKey, &types.DynamicFeeTx{
		Nonce: 0, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault, Value: big.NewInt(5e17),
	})
	dep := detectedDeposit{BlockHeight: 101, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
	payload := buildMapPayloadFromRPC(context.Background(), c.rpc(t), dep, dep.BlockHeight)
	var p contractMapParams
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, _ := tx.MarshalBinary()
	if p.TxData.RawHex != hex.EncodeToString(raw) {
		t.Fatalf("raw_hex is not the transaction")
	}
	verifyLeaf(t, "eth tx", c.txRoot(), 0, p.TxData.RawHex, p.TxData.MerkleProofHex)
}

// A tx value is a uint256. The scanner read it with hexToUint64, which keeps
// only the low 64 bits, so an ETH deposit of an exact multiple of 2^64 wei
// (e.g. exactly 18.446744073709551616 ETH) read as 0 and was never mapped.
func TestScanBlock_DetectsETHDepositAtMultipleOf2To64Wei(t *testing.T) {
	vault := common.HexToAddress("0x00000000000000000000000000000000000000fa")
	userKey, _ := ethCrypto.GenerateKey()
	twoTo64 := new(big.Int).Lsh(big.NewInt(1), 64)
	c := &fakeChain{}
	for i, v := range []*big.Int{twoTo64, big.NewInt(1), big.NewInt(0)} {
		c.addTx(t, userKey, &types.DynamicFeeTx{
			Nonce: uint64(i), GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault, Value: v,
		})
	}
	res, err := scanBlock(c.rpc(t), 101, []string{vault.Hex()}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, d := range res.Deposits {
		got = append(got, d.TxIndex)
	}
	if fmt.Sprint(got) != "[0 1]" {
		t.Fatalf("deposits detected at tx %v, want [0 1] (2^64 wei and 1 wei; not the 0-value tx)", got)
	}
}
