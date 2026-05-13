package state_engine

import (
	"errors"
	"testing"
	contract_session "vsc-node/modules/contract/session"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
)

// ResetSlotState is the load-bearing primitive for halt-retry determinism:
// it must clear every in-memory accumulator that grows by append during
// slot execution, otherwise the next replay sees ghost contributions from
// the failed attempt and diverges. These tests pin every field the function
// is responsible for, so a future field added to a state-engine accumulator
// without being added to ResetSlotState will trip the assertions here.

// populateAllAccumulators fills every field that ResetSlotState clears with
// non-zero values. Returns the engine for further inspection.
func populateAllAccumulators(slotHeight uint64) *StateEngine {
	se := &StateEngine{
		LedgerState: &ledgerSystem.LedgerState{},
	}

	se.ContractResults = map[string][]ContractResult{
		"contract-1": {{TxId: "tx-1", Success: true}},
	}
	se.TempOutputs = map[string]*contract_session.TempOutput{
		"contract-1": {Cid: "cached-state", Cache: map[string][]byte{"k": []byte("v")}, Deletions: map[string]bool{}},
	}
	se.TxOutput = map[string]TxOutput{
		"tx-1": {Ok: true, LedgerIds: []string{"ledger-1"}},
	}
	se.TxOutIds = []string{"tx-1", "tx-2"}
	se.firstTxHeight = slotHeight + 3
	se.TxBatch = []TxPacket{{TxId: "queued-tx-1"}, {TxId: "queued-tx-2"}}
	se.RcMap = map[string]int64{"alice": 100}
	se.slotStatus = &SlotStatus{SlotHeight: slotHeight, Done: false}

	// LedgerState accumulators feed ledgerSession.GetBalance during
	// ExecuteBatch, so they're equally critical to clear.
	se.LedgerState.Oplog = []ledgerSystem.OpLogEvent{
		{Id: "oplog-1", From: "hive:alice", To: "hive:bob", Amount: 50, Asset: "hbd"},
	}
	se.LedgerState.VirtualLedger = map[string][]ledgerSystem.LedgerUpdate{
		"hive:alice": {{Id: "lu-1", Owner: "hive:alice", Amount: -50, Asset: "hbd"}},
	}
	se.LedgerState.GatewayBalances = map[string]uint64{"hive:alice": 100}

	se.unsafeFetchErr.Store(&unsafeHaltErr{
		site: "TxProposeBlock.AsTransaction",
		err:  errors.New("simulated unreachable CID"),
	})

	return se
}

func TestResetSlotState_ClearsAllAccumulators(t *testing.T) {
	const slotHeight = uint64(100)
	se := populateAllAccumulators(slotHeight)

	got, ok := se.ResetSlotState()

	assert.True(t, ok, "should report ok=true when slotStatus was set")
	assert.Equal(t, slotHeight, got, "should return the slot start to replay from")

	// State-engine accumulators (also flushed by Flush()):
	assert.Empty(t, se.ContractResults, "ContractResults must be cleared")
	assert.Empty(t, se.TempOutputs, "TempOutputs must be cleared")
	assert.Empty(t, se.TxOutput, "TxOutput must be cleared")
	assert.Empty(t, se.TxOutIds, "TxOutIds must be cleared — duplicates here would corrupt MakeOplog")
	assert.Equal(t, uint64(0), se.firstTxHeight, "firstTxHeight must be zeroed")

	// Cleared exclusively by ResetSlotState:
	assert.Empty(t, se.TxBatch, "TxBatch must be cleared — duplicates here would re-execute user ops on replay")
	assert.Empty(t, se.RcMap, "RcMap must be cleared")
	assert.Nil(t, se.slotStatus, "slotStatus must be nil so the next ProcessBlock re-initializes from current slotInfo")

	// LedgerState accumulators (drive GetBalance via SnapshotForAccount):
	assert.Empty(t, se.LedgerState.Oplog, "LedgerState.Oplog must be cleared — duplicates here flip tx Ok/Failed via inflated wet balances")
	assert.Empty(t, se.LedgerState.VirtualLedger, "LedgerState.VirtualLedger must be cleared")
	assert.Empty(t, se.LedgerState.GatewayBalances, "LedgerState.GatewayBalances must be cleared")

	// Halt-error state must be dropped so the next replay starts clean.
	assert.Nil(t, se.unsafeFetchErr.Load(), "unsafeFetchErr must be cleared so the next halt-check is unaffected")
	assert.Nil(t, se.ConsumeUnsafeHalt(), "ConsumeUnsafeHalt must report no halt after reset")
}

func TestResetSlotState_NoActiveSlot(t *testing.T) {
	// Process just started, no block fed yet — slotStatus is nil. Reset
	// should be a no-op for the slot-start return value, and the streamer
	// will fall back to retrying just the failing block.
	se := &StateEngine{LedgerState: &ledgerSystem.LedgerState{}}

	got, ok := se.ResetSlotState()

	assert.False(t, ok, "should report ok=false when no slot is active")
	assert.Equal(t, uint64(0), got)
}

func TestResetSlotState_IsIdempotent(t *testing.T) {
	// Halt-retry can fire haltReset multiple times in a row if the streamer's
	// outer loop receives multiple halts back-to-back. The second call must
	// be a clean no-op rather than racing or panicking.
	se := populateAllAccumulators(200)

	first, ok1 := se.ResetSlotState()
	second, ok2 := se.ResetSlotState()

	assert.True(t, ok1)
	assert.Equal(t, uint64(200), first)
	assert.False(t, ok2, "second reset has no slot to report")
	assert.Equal(t, uint64(0), second)
}

func TestResetSlotState_ReturnsSlotHeightNotFirstTxHeight(t *testing.T) {
	// firstTxHeight - 1 is the SaveBlockHeight pin point (the block before
	// the slot's first user op); slotStatus.SlotHeight is the actual slot
	// boundary. Replay must start from the slot boundary so prior in-slot
	// blocks (including those before the first user op) re-feed and any
	// non-user-op state contributions are reproduced.
	se := populateAllAccumulators(150)
	se.firstTxHeight = 155 // first user op at block 156

	got, _ := se.ResetSlotState()

	assert.Equal(t, uint64(150), got, "must return slotStatus.SlotHeight, not firstTxHeight or pin point")
}
