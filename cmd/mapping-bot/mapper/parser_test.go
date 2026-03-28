package mapper

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"

	"vsc-node/cmd/mapping-bot/chain"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// makeCoinbaseTx creates a minimal coinbase transaction.
func makeCoinbaseTx() *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0xffffffff,
		},
		SignatureScript: []byte{0x04, 0xff, 0xff, 0x00, 0x1d, 0x01, 0x04},
		Sequence:        0xffffffff,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    5000000000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	return tx
}

// makePayToAddrTx creates a transaction with a single output paying to the
// given bech32 address on the provided network.
func makePayToAddrTx(t *testing.T, address string, params *chaincfg.Params) *wire.MsgTx {
	t.Helper()
	addr, err := btcutil.DecodeAddress(address, params)
	require.NoError(t, err, "failed to decode address %q", address)
	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err, "failed to build pkScript for %q", address)

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{0xab}, Index: 0},
		Sequence:         0xffffffff,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    100_000,
		PkScript: pkScript,
	})
	return tx
}

// makeRandomTx creates a transaction whose hash is unique (uses the seed to
// vary the coinbase-style input script).
func makeRandomTx(seed byte) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{seed}, Index: 0},
		Sequence:         0xffffffff,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    50_000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	return tx
}

// buildBlock constructs a wire.MsgBlock from a list of transactions and
// serialises it to bytes.
func buildBlock(t *testing.T, txs []*wire.MsgTx) []byte {
	t.Helper()
	block := &wire.MsgBlock{
		Header: wire.BlockHeader{
			Version:   1,
			PrevBlock: chainhash.Hash{},
			Bits:      0x1d00ffff,
			Nonce:     0,
		},
		Transactions: txs,
	}
	// The MerkleRoot field doesn't affect deserialization, but let's set it
	// properly for completeness.
	if len(txs) > 0 {
		hashes := make([]*chainhash.Hash, len(txs))
		for i, tx := range txs {
			h := tx.TxHash()
			hashes[i] = &h
		}
		block.Header.MerkleRoot = *hashes[0] // placeholder; not checked
	}
	var buf bytes.Buffer
	require.NoError(t, block.Serialize(&buf))
	return buf.Bytes()
}

// generateDepositAddress is a small wrapper that produces a deposit address
// and its witness script for the given instruction using the well-known test
// keys.
func generateDepositAddress(
	t *testing.T, instruction string, params *chaincfg.Params,
) string {
	t.Helper()
	gen := &chain.BTCAddressGenerator{Params: params, BackupCSVBlocks: 2}
	addr, _, err := gen.GenerateDepositAddress(
		testPrimaryKeyHex, testBackupKeyHex, instruction,
	)
	require.NoError(t, err)
	return addr
}

// Compressed secp256k1 public keys used only for tests (generator point G and 2G).
const (
	testPrimaryKeyHex = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	testBackupKeyHex  = "02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
)

// regtest is the chain params used across all tests in this file.
var regtest = &chaincfg.RegressionNetParams

// ---------------------------------------------------------------------------
// generateMerkleProof tests
// ---------------------------------------------------------------------------

func TestGenerateMerkleProof_SingleTx(t *testing.T) {
	coinbase := makeCoinbaseTx()
	block := &wire.MsgBlock{
		Header:       wire.BlockHeader{Version: 1, Bits: 0x1d00ffff},
		Transactions: []*wire.MsgTx{coinbase},
	}

	proofHex, err := generateMerkleProof(block, 0)
	require.NoError(t, err)
	assert.Empty(t, proofHex, "single-tx block should have an empty merkle proof")
}

func TestGenerateMerkleProof_TwoTxs(t *testing.T) {
	tx0 := makeCoinbaseTx()
	tx1 := makeRandomTx(0x01)

	block := &wire.MsgBlock{
		Header:       wire.BlockHeader{Version: 1, Bits: 0x1d00ffff},
		Transactions: []*wire.MsgTx{tx0, tx1},
	}

	// Proof for tx at index 0 — sibling is tx1
	proof0, err := generateMerkleProof(block, 0)
	require.NoError(t, err)
	proofBytes0, err := hex.DecodeString(proof0)
	require.NoError(t, err)
	assert.Len(t, proofBytes0, 32, "two-tx proof should contain exactly 1 hash (32 bytes)")

	// The single proof element should be the hash of the sibling (tx1)
	tx1Hash := tx1.TxHash()
	assert.Equal(t, tx1Hash[:], proofBytes0, "sibling hash should be tx1's txhash")

	// Proof for tx at index 1 — sibling is tx0
	proof1, err := generateMerkleProof(block, 1)
	require.NoError(t, err)
	proofBytes1, err := hex.DecodeString(proof1)
	require.NoError(t, err)
	assert.Len(t, proofBytes1, 32)
	tx0Hash := tx0.TxHash()
	assert.Equal(t, tx0Hash[:], proofBytes1)
}

func TestGenerateMerkleProof_MultipleTxs(t *testing.T) {
	// Build a block with 5 transactions: 1 coinbase + 4 regular.
	// 5 txs => tree levels: 5 -> 3 -> 2 -> 1 => 3 proof elements (depth = ceil(log2(5)))
	txs := []*wire.MsgTx{makeCoinbaseTx()}
	for i := byte(1); i <= 4; i++ {
		txs = append(txs, makeRandomTx(i))
	}
	block := &wire.MsgBlock{
		Header:       wire.BlockHeader{Version: 1, Bits: 0x1d00ffff},
		Transactions: txs,
	}

	expectedDepth := int(math.Ceil(math.Log2(float64(len(txs)))))

	for txIdx := 0; txIdx < len(txs); txIdx++ {
		proofHex, err := generateMerkleProof(block, txIdx)
		require.NoError(t, err, "txIdx=%d", txIdx)
		proofBytes, err := hex.DecodeString(proofHex)
		require.NoError(t, err, "txIdx=%d", txIdx)

		numHashes := len(proofBytes) / 32
		assert.Equal(t, expectedDepth, numHashes,
			"txIdx=%d: expected %d proof hashes but got %d", txIdx, expectedDepth, numHashes)
	}

	// Also test with exactly 4 transactions (power of 2).
	txs4 := txs[:4]
	block4 := &wire.MsgBlock{
		Header:       wire.BlockHeader{Version: 1, Bits: 0x1d00ffff},
		Transactions: txs4,
	}
	expectedDepth4 := 2 // log2(4) = 2
	for txIdx := 0; txIdx < len(txs4); txIdx++ {
		proofHex, err := generateMerkleProof(block4, txIdx)
		require.NoError(t, err, "4-tx block txIdx=%d", txIdx)
		proofBytes, err := hex.DecodeString(proofHex)
		require.NoError(t, err, "4-tx block txIdx=%d", txIdx)
		numHashes := len(proofBytes) / 32
		assert.Equal(t, expectedDepth4, numHashes,
			"4-tx block txIdx=%d: expected %d proof hashes but got %d", txIdx, expectedDepth4, numHashes)
	}
}

// ---------------------------------------------------------------------------
// ParseBlock tests (using chain.BTCBlockParser — the pure-function version)
// ---------------------------------------------------------------------------

func TestParseBlock_MatchesKnownAddress(t *testing.T) {
	instruction := "deposit_to=hive:testuser"
	depositAddr := generateDepositAddress(t, instruction, regtest)

	coinbase := makeCoinbaseTx()
	payTx := makePayToAddrTx(t, depositAddr, regtest)

	rawBlock := buildBlock(t, []*wire.MsgTx{coinbase, payTx})

	parser := &chain.BTCBlockParser{Params: regtest}
	results, err := parser.ParseBlock(rawBlock, []string{depositAddr}, 42000)
	require.NoError(t, err)
	require.Len(t, results, 1, "expected exactly 1 matched transaction")

	m := results[0]
	assert.Equal(t, uint32(42000), m.BlockHeight)
	assert.Equal(t, uint32(1), m.TxIndex, "matched tx should be at index 1 (after coinbase)")
	assert.NotEmpty(t, m.RawTxHex, "RawTxHex should be populated")

	// Verify the raw tx hex decodes back to a valid transaction
	txBytes, err := hex.DecodeString(m.RawTxHex)
	require.NoError(t, err)
	var decodedTx wire.MsgTx
	require.NoError(t, decodedTx.Deserialize(bytes.NewReader(txBytes)))
	assert.Equal(t, payTx.TxHash(), decodedTx.TxHash(), "decoded tx should match the original")

	// Merkle proof should have exactly 1 hash (2 txs in block)
	proofBytes, err := hex.DecodeString(m.MerkleProofHex)
	require.NoError(t, err)
	assert.Len(t, proofBytes, 32, "merkle proof should have 1 hash for a 2-tx block")
}

func TestParseBlock_NoMatchingAddress(t *testing.T) {
	coinbase := makeCoinbaseTx()
	randomTx1 := makeRandomTx(0x10)
	randomTx2 := makeRandomTx(0x20)

	rawBlock := buildBlock(t, []*wire.MsgTx{coinbase, randomTx1, randomTx2})

	// Use a deposit address that none of the transactions pay to
	depositAddr := generateDepositAddress(t, "deposit_to=hive:nobody", regtest)

	parser := &chain.BTCBlockParser{Params: regtest}
	results, err := parser.ParseBlock(rawBlock, []string{depositAddr}, 100)
	require.NoError(t, err)
	assert.Empty(t, results, "no transactions should match")
}

func TestParseBlock_MultipleMatchingTxs(t *testing.T) {
	instr1 := "deposit_to=hive:alice"
	instr2 := "deposit_to=hive:bob"
	addr1 := generateDepositAddress(t, instr1, regtest)
	addr2 := generateDepositAddress(t, instr2, regtest)

	coinbase := makeCoinbaseTx()
	payTx1 := makePayToAddrTx(t, addr1, regtest)
	payTx2 := makePayToAddrTx(t, addr2, regtest)
	// Add a non-matching tx in between
	noise := makeRandomTx(0x99)

	rawBlock := buildBlock(t, []*wire.MsgTx{coinbase, payTx1, noise, payTx2})

	parser := &chain.BTCBlockParser{Params: regtest}
	results, err := parser.ParseBlock(rawBlock, []string{addr1, addr2}, 500)
	require.NoError(t, err)
	require.Len(t, results, 2, "expected 2 matched transactions")

	// Collect matched indices
	matchedIndices := map[uint32]bool{}
	for _, m := range results {
		matchedIndices[m.TxIndex] = true
		assert.Equal(t, uint32(500), m.BlockHeight)
		assert.NotEmpty(t, m.RawTxHex)
		assert.NotEmpty(t, m.MerkleProofHex)
	}
	assert.True(t, matchedIndices[1], "payTx1 at index 1 should be matched")
	assert.True(t, matchedIndices[3], "payTx2 at index 3 should be matched")
}

// ---------------------------------------------------------------------------
// Address generation tests
// ---------------------------------------------------------------------------

func TestAddressGeneration(t *testing.T) {
	gen := &chain.BTCAddressGenerator{
		Params:          regtest,
		BackupCSVBlocks: 2,
	}

	instruction := "deposit_to=hive:vaultec"

	addr, witnessScript, err := gen.GenerateDepositAddress(
		testPrimaryKeyHex, testBackupKeyHex, instruction,
	)
	require.NoError(t, err)
	assert.NotEmpty(t, addr)
	assert.NotEmpty(t, witnessScript)

	// Verify the address is valid bech32 on regtest
	decoded, err := btcutil.DecodeAddress(addr, regtest)
	require.NoError(t, err, "generated address should be valid for regtest")
	assert.True(t, decoded.IsForNet(regtest), "address should be for regtest network")

	// Verify the address is a P2WSH (witness script hash)
	_, ok := decoded.(*btcutil.AddressWitnessScriptHash)
	assert.True(t, ok, "address should be a P2WSH type")

	// The witness program should be SHA256 of the witness script
	expectedWitnessProgram := sha256.Sum256(witnessScript)
	wsh := decoded.(*btcutil.AddressWitnessScriptHash)
	assert.Equal(t, expectedWitnessProgram[:], wsh.ScriptAddress(),
		"witness program should equal SHA256 of the witness script")
}

func TestAddressGeneration_DeterministicAndUnique(t *testing.T) {
	gen := &chain.BTCAddressGenerator{
		Params:          regtest,
		BackupCSVBlocks: 2,
	}

	// Same inputs should produce same output (deterministic)
	addr1, _, err := gen.GenerateDepositAddress(testPrimaryKeyHex, testBackupKeyHex, "instr-A")
	require.NoError(t, err)
	addr2, _, err := gen.GenerateDepositAddress(testPrimaryKeyHex, testBackupKeyHex, "instr-A")
	require.NoError(t, err)
	assert.Equal(t, addr1, addr2, "same inputs must produce the same address")

	// Different instructions should produce different addresses
	addr3, _, err := gen.GenerateDepositAddress(testPrimaryKeyHex, testBackupKeyHex, "instr-B")
	require.NoError(t, err)
	assert.NotEqual(t, addr1, addr3, "different instructions must produce different addresses")
}

func TestAddressGeneration_DifferentNetworks(t *testing.T) {
	instruction := "deposit_to=hive:alice"

	genRegtest := &chain.BTCAddressGenerator{Params: regtest, BackupCSVBlocks: 2}
	genTestnet := &chain.BTCAddressGenerator{Params: &chaincfg.TestNet3Params, BackupCSVBlocks: 2}

	addrRegtest, _, err := genRegtest.GenerateDepositAddress(
		testPrimaryKeyHex, testBackupKeyHex, instruction,
	)
	require.NoError(t, err)

	addrTestnet, _, err := genTestnet.GenerateDepositAddress(
		testPrimaryKeyHex, testBackupKeyHex, instruction,
	)
	require.NoError(t, err)

	// Same script content, but different HRP means different encoded addresses
	assert.NotEqual(t, addrRegtest, addrTestnet,
		"regtest and testnet addresses should differ due to HRP")

	// Verify each is valid for its network
	decodedR, err := btcutil.DecodeAddress(addrRegtest, regtest)
	require.NoError(t, err)
	assert.True(t, decodedR.IsForNet(regtest))

	decodedT, err := btcutil.DecodeAddress(addrTestnet, &chaincfg.TestNet3Params)
	require.NoError(t, err)
	assert.True(t, decodedT.IsForNet(&chaincfg.TestNet3Params))
}
