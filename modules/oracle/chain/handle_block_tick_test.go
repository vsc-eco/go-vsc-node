package chain

import (
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
