package chain

import (
	"testing"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/elections"

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

func TestMakeTransactionPayload(t *testing.T) {
	blocks := []chainBlock{
		&mockChainBlock{height: 100, data: "aabbccdd"},
		&mockChainBlock{height: 101, data: "11223344"},
		&mockChainBlock{height: 102, data: "deadbeef"},
	}

	payload, err := makeTransactionPayload(blocks)
	assert.NoError(t, err)
	assert.Equal(t, 3, len(payload))
	assert.Equal(t, "aabbccdd", payload[0])
	assert.Equal(t, "11223344", payload[1])
	assert.Equal(t, "deadbeef", payload[2])
}

func TestMakeTransactionPayload_Empty(t *testing.T) {
	payload, err := makeTransactionPayload([]chainBlock{})
	assert.NoError(t, err)
	assert.Equal(t, 0, len(payload))
}

func TestMakeTransaction(t *testing.T) {
	tx := makeTransaction("vsc1contract", `["aabb","ccdd"]`, "BTC", "vsc-mocknet")

	assert.Equal(t, 1, len(tx.Ops))
	assert.Equal(t, "call", tx.Ops[0].Type)
	assert.Equal(t, uint64(0), tx.Nonce)
	assert.Equal(t, "vsc-mocknet", tx.NetId)
}

func TestMakeTransaction_CallerFormat(t *testing.T) {
	tx := makeTransaction("vsc1x", `["data"]`, "BTC", "vsc-mocknet")

	// The caller should be lowercase oracle DID
	assert.Contains(t, tx.Ops[0].RequiredAuths.Active, "did:vsc:oracle:btc")
}

func TestMakeTransaction_DashSymbol(t *testing.T) {
	tx := makeTransaction("vsc1y", `["data"]`, "DASH", "vsc-mocknet")
	assert.Contains(t, tx.Ops[0].RequiredAuths.Active, "did:vsc:oracle:dash")
}
