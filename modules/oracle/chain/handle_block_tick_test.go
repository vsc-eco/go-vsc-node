package chain

import (
	"encoding/binary"
	"math/big"
	"testing"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/elections"

	"github.com/btcsuite/btcd/wire"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
)

func TestFindMemberWeight(t *testing.T) {
	election := &elections.ElectionResult{
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Key: "did:key:z6Alice", Account: "alice"},
				{Key: "did:key:z6Bob", Account: "bob"},
				{Key: "did:key:z6Carol", Account: "carol"},
			},
			Weights: []uint64{10, 20, 30},
		},
		TotalWeight: 60,
	}

	assert.Equal(t, uint64(10), findMemberWeight(election, dids.BlsDID("did:key:z6Alice")))
	assert.Equal(t, uint64(20), findMemberWeight(election, dids.BlsDID("did:key:z6Bob")))
	assert.Equal(t, uint64(30), findMemberWeight(election, dids.BlsDID("did:key:z6Carol")))
}

func TestFindMemberWeight_NotFound(t *testing.T) {
	election := &elections.ElectionResult{
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Key: "did:key:z6Alice", Account: "alice"},
			},
			Weights: []uint64{10},
		},
	}

	assert.Equal(t, uint64(0), findMemberWeight(election, dids.BlsDID("did:key:z6Unknown")))
}

func TestFindMemberWeight_MissingWeights(t *testing.T) {
	// If weights array is shorter than members, fall back to 1
	election := &elections.ElectionResult{
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Key: "did:key:z6Alice", Account: "alice"},
				{Key: "did:key:z6Bob", Account: "bob"},
			},
			Weights: []uint64{10},
		},
	}

	assert.Equal(t, uint64(10), findMemberWeight(election, dids.BlsDID("did:key:z6Alice")))
	// Bob's index (1) >= len(Weights), so defaults to 1
	assert.Equal(t, uint64(1), findMemberWeight(election, dids.BlsDID("did:key:z6Bob")))
}

func TestMakeTransactionPayload_UTXO(t *testing.T) {
	blocks := []chainBlock{
		&mockChainBlock{height: 100, data: "aabbccdd"},
		&mockChainBlock{height: 101, data: "11223344"},
		&mockChainBlock{height: 102, data: "deadbeef"},
	}

	raw, err := makeTransactionPayload(blocks)
	assert.NoError(t, err)
	payload := raw.(*utxoAddBlocksPayload)
	assert.Equal(t, "aabbccdd11223344deadbeef", payload.Blocks)
	assert.Equal(t, int64(0), payload.LatestFee) // non-BTC blocks have no fee
}

func TestMakeTransactionPayload_Empty(t *testing.T) {
	raw, err := makeTransactionPayload([]chainBlock{})
	assert.NoError(t, err)
	payload := raw.(*utxoAddBlocksPayload)
	assert.Equal(t, "", payload.Blocks)
}

func TestMakeTransactionPayload_BTCFeeRate(t *testing.T) {
	blocks := []chainBlock{
		&btcChainData{Height: 100, AverageFeeRate: 5, blockHeader: &wire.BlockHeader{}},
		&btcChainData{Height: 101, AverageFeeRate: 12, blockHeader: &wire.BlockHeader{}},
	}

	raw, err := makeTransactionPayload(blocks)
	assert.NoError(t, err)
	payload := raw.(*utxoAddBlocksPayload)
	assert.Equal(t, int64(12), payload.LatestFee)
}

func TestMakeTransactionPayload_ETH(t *testing.T) {
	blocks := []chainBlock{
		&ethChainData{
			Height: 100,
			header: &types.Header{
				TxHash:      common.HexToHash("aabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccdd"),
				ReceiptHash: common.HexToHash("1122334411223344112233441122334411223344112233441122334411223344"),
				BaseFee:     big.NewInt(1000000000),
				GasLimit:    30000000,
				Time:        1700000000,
			},
		},
	}

	raw, err := makeTransactionPayload(blocks)
	assert.NoError(t, err)
	payload := raw.(*ethAddBlocksPayload)
	assert.Equal(t, 1, len(payload.Blocks))
	assert.Equal(t, uint64(100), payload.Blocks[0].BlockNumber)
	assert.Equal(t, "aabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccdd", payload.Blocks[0].TransactionsRoot)
	assert.Equal(t, "1122334411223344112233441122334411223344112233441122334411223344", payload.Blocks[0].ReceiptsRoot)
	assert.Equal(t, uint64(1000000000), payload.Blocks[0].BaseFeePerGas)
	assert.Equal(t, uint64(30000000), payload.Blocks[0].GasLimit)
	assert.Equal(t, uint64(1700000000), payload.Blocks[0].Timestamp)
	assert.Equal(t, uint64(1000000000), payload.LatestFee)
}

// EVM-C2: assert that makeEthPayload sets each entry's parent_hash
// from the ethChainData's ParentHashContract field, and that
// ParentHashContract chains forward correctly when ChainData
// produces a multi-block batch (each entry's parent must equal the
// keccak256 of its predecessor's contract-format serialization).
//
// Pre-fix: makeEthPayload silently dropped the chain link → contract
// rejects every addBlocks call with "parent_hash mismatch".
func TestMakeTransactionPayload_ETH_ParentHashChainLink(t *testing.T) {
	// Build three sequential headers. ParentHashContract for entry[0]
	// is whatever the previous tip's hash is; for entry[1] it must be
	// the contract-format keccak of header[0]; for entry[2] it must be
	// the contract-format keccak of header[1].
	h100 := &types.Header{
		Number:      big.NewInt(100),
		Root:        common.HexToHash("aa00000000000000000000000000000000000000000000000000000000000000"),
		TxHash:      common.HexToHash("aa11000000000000000000000000000000000000000000000000000000000000"),
		ReceiptHash: common.HexToHash("aa22000000000000000000000000000000000000000000000000000000000000"),
		BaseFee:     big.NewInt(1_000_000_000),
		GasLimit:    30_000_000,
		Time:        1_700_000_000,
	}
	h101 := &types.Header{
		Number:      big.NewInt(101),
		Root:        common.HexToHash("bb00000000000000000000000000000000000000000000000000000000000000"),
		TxHash:      common.HexToHash("bb11000000000000000000000000000000000000000000000000000000000000"),
		ReceiptHash: common.HexToHash("bb22000000000000000000000000000000000000000000000000000000000000"),
		BaseFee:     big.NewInt(1_100_000_000),
		GasLimit:    30_000_000,
		Time:        1_700_000_012,
	}
	h102 := &types.Header{
		Number:      big.NewInt(102),
		Root:        common.HexToHash("cc00000000000000000000000000000000000000000000000000000000000000"),
		TxHash:      common.HexToHash("cc11000000000000000000000000000000000000000000000000000000000000"),
		ReceiptHash: common.HexToHash("cc22000000000000000000000000000000000000000000000000000000000000"),
		BaseFee:     big.NewInt(1_050_000_000),
		GasLimit:    30_000_000,
		Time:        1_700_000_024,
	}

	prevTipHash, err := contractFormatKeccakHex(h100)
	assert.NoError(t, err)
	h101Hash, err := contractFormatKeccakHex(h101)
	assert.NoError(t, err)

	blocks := []chainBlock{
		&ethChainData{Height: 101, header: h101, ParentHashContract: prevTipHash},
		&ethChainData{Height: 102, header: h102, ParentHashContract: h101Hash},
	}

	raw, err := makeTransactionPayload(blocks)
	assert.NoError(t, err)
	payload := raw.(*ethAddBlocksPayload)
	assert.Len(t, payload.Blocks, 2)

	// Entry 0 must reference the previous tip's contract-format hash.
	assert.Equal(t, prevTipHash, payload.Blocks[0].ParentHash,
		"entry[0].parent_hash must equal keccak256(serialize(prev tip))")
	// Entry 1's parent_hash must equal the contract-format keccak of
	// the previous entry — i.e. it chains forward.
	assert.Equal(t, h101Hash, payload.Blocks[1].ParentHash,
		"entry[1].parent_hash must equal keccak256(serialize(prev entry))")

	// Sanity: parent_hash is non-empty (regression check — pre-fix
	// the field didn't exist on the JSON output).
	assert.NotEmpty(t, payload.Blocks[0].ParentHash)
	assert.NotEmpty(t, payload.Blocks[1].ParentHash)
}

// Pin the contract-format byte layout so any drift between this side
// and evm-mapping-contract's EthBlockHeader.Serialize() trips the
// test rather than producing a confusing parent_hash-mismatch error
// at runtime.
func TestSerializeContractFormat_ByteLayout(t *testing.T) {
	var stateRoot, txRoot, rcptRoot [32]byte
	for i := range stateRoot {
		stateRoot[i] = 0x11
		txRoot[i] = 0x22
		rcptRoot[i] = 0x33
	}
	buf := serializeContractFormat(uint64(100), stateRoot, txRoot, rcptRoot, uint64(1_000_000_000), uint64(30_000_000), uint64(1_700_000_000))
	assert.Len(t, buf, 128, "contract format must be exactly 128 bytes")
	// 8-byte BE block number at offset 0
	assert.Equal(t, uint64(100), binary.BigEndian.Uint64(buf[0:8]))
	// state_root at offset 8 (32 bytes)
	for i := 0; i < 32; i++ {
		assert.Equal(t, byte(0x11), buf[8+i])
	}
	// tx_root at offset 40
	for i := 0; i < 32; i++ {
		assert.Equal(t, byte(0x22), buf[40+i])
	}
	// receipts_root at offset 72
	for i := 0; i < 32; i++ {
		assert.Equal(t, byte(0x33), buf[72+i])
	}
	// base fee at 104, gas limit at 112, timestamp at 120
	assert.Equal(t, uint64(1_000_000_000), binary.BigEndian.Uint64(buf[104:112]))
	assert.Equal(t, uint64(30_000_000), binary.BigEndian.Uint64(buf[112:120]))
	assert.Equal(t, uint64(1_700_000_000), binary.BigEndian.Uint64(buf[120:128]))
}

func TestMakeTransactionPayload_ETHNilBaseFee(t *testing.T) {
	blocks := []chainBlock{
		&ethChainData{
			Height: 101,
			header: &types.Header{
				TxHash:      common.HexToHash("bbbbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccdd"),
				ReceiptHash: common.HexToHash("2222334411223344112233441122334411223344112233441122334411223344"),
				BaseFee:     nil,
				GasLimit:    31000000,
				Time:        1700000001,
			},
		},
	}

	raw, err := makeTransactionPayload(blocks)
	assert.NoError(t, err)
	payload := raw.(*ethAddBlocksPayload)
	assert.Equal(t, uint64(0), payload.Blocks[0].BaseFeePerGas)
	assert.Equal(t, uint64(0), payload.LatestFee)
}

func TestMakeTransactionPayload_ETHBaseFeeOverflow(t *testing.T) {
	overflowFee := new(big.Int).Lsh(big.NewInt(1), 65)
	blocks := []chainBlock{
		&ethChainData{
			Height: 102,
			header: &types.Header{
				TxHash:      common.HexToHash("ccccccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccdd"),
				ReceiptHash: common.HexToHash("3333334411223344112233441122334411223344112233441122334411223344"),
				BaseFee:     overflowFee,
				GasLimit:    32000000,
				Time:        1700000002,
			},
		},
	}

	_, err := makeTransactionPayload(blocks)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "base fee out of uint64 range")
}

func TestMakeTransaction(t *testing.T) {
	tx := makeTransaction("vsc1contract", `["aabb","ccdd"]`, "addBlocks", "BTC", "vsc-mocknet", 0)

	assert.Equal(t, 1, len(tx.Ops))
	assert.Equal(t, "call", tx.Ops[0].Type)
	assert.Equal(t, uint64(0), tx.Nonce)
	assert.Equal(t, "vsc-mocknet", tx.NetId)
}

func TestMakeTransaction_CallerFormat(t *testing.T) {
	tx := makeTransaction("vsc1x", `["data"]`, "addBlocks", "BTC", "vsc-mocknet", 0)

	// The caller should be lowercase oracle DID
	assert.Contains(t, tx.Ops[0].RequiredAuths.Active, "did:vsc:oracle:btc")
}

func TestMakeTransaction_DashSymbol(t *testing.T) {
	tx := makeTransaction("vsc1y", `["data"]`, "addBlocks", "DASH", "vsc-mocknet", 0)
	assert.Contains(t, tx.Ops[0].RequiredAuths.Active, "did:vsc:oracle:dash")
}
