package state_engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// SaveBlockHeight is small enough that direct construction beats the full
// testEnv harness — we just need TxOutput / firstTxHeight populated.

func newSEWithTxOutput(firstTxHeight uint64, n int) *StateEngine {
	se := &StateEngine{
		TxOutput:        make(map[string]TxOutput),
		ContractResults: make(map[string][]ContractResult),
		TxOutIds:        make([]string, 0),
	}
	for i := 0; i < n; i++ {
		se.TxOutput[string(rune('a'+i))] = TxOutput{}
	}
	se.firstTxHeight = firstTxHeight
	return se
}

func TestSaveBlockHeight_ZeroInputsReturnsLastSaved(t *testing.T) {
	se := newSEWithTxOutput(0, 0)

	// When either input is 0 the function short-circuits and returns
	// lastSavedBlk verbatim — guards Init's pre-startup state.
	assert.Equal(t, uint64(0), se.SaveBlockHeight(0, 0))
	assert.Equal(t, uint64(42), se.SaveBlockHeight(0, 42))
	assert.Equal(t, uint64(0), se.SaveBlockHeight(100, 0))
}

func TestSaveBlockHeight_NoTxOutputAdvances(t *testing.T) {
	se := newSEWithTxOutput(0, 0)

	got := se.SaveBlockHeight(1000, 500)
	assert.Equal(t, uint64(1000), got, "with no pending TxOutput, lastBlk should pass through")
}

func TestSaveBlockHeight_PinsWithinWindow(t *testing.T) {
	// firstTxHeight=100, lastBlk well within 2*SlotLength of pinHeight (=99)
	se := newSEWithTxOutput(100, 1)

	pin := uint64(99) // firstTxHeight - 1
	lastBlk := pin + CONSENSUS_SPECS.SlotLength // half a window away

	got := se.SaveBlockHeight(lastBlk, 200)
	assert.Equal(t, pin, got, "should pin to firstTxHeight-1 within the window")

	// State should NOT be mutated while pinning.
	assert.Equal(t, uint64(100), se.firstTxHeight)
	assert.Len(t, se.TxOutput, 1)
}

func TestSaveBlockHeight_PinExpiresAndClears(t *testing.T) {
	// firstTxHeight=100, lastBlk well past 2*SlotLength of pinHeight (=99)
	se := newSEWithTxOutput(100, 3)
	se.ContractResults["c"] = []ContractResult{{}}
	se.TxOutIds = []string{"tx-keep"}

	pin := uint64(99)
	lastBlk := pin + 2*CONSENSUS_SPECS.SlotLength + 1 // one past the window

	got := se.SaveBlockHeight(lastBlk, 200)
	assert.Equal(t, lastBlk, got, "past-window should advance to lastBlk")

	// Pin-driving fields cleared.
	assert.Empty(t, se.TxOutput, "TxOutput must be cleared when pin window expires")
	assert.Equal(t, uint64(0), se.firstTxHeight, "firstTxHeight must be cleared with TxOutput")

	// Cross-batch fields preserved (the partial-clear is intentional;
	// Flush clears these and would corrupt mid-slot oplog/contract output).
	assert.Equal(t, []string{"tx-keep"}, se.TxOutIds, "TxOutIds must NOT be cleared — needed for next MakeOplog")
	assert.Len(t, se.ContractResults["c"], 1, "ContractResults must NOT be cleared — needed for cross-batch contract state")
}

func TestSaveBlockHeight_PinExactlyAtBoundary(t *testing.T) {
	// lastBlk - pinHeight == 2*SlotLength : still within (uses <=)
	se := newSEWithTxOutput(100, 1)
	pin := uint64(99)
	lastBlk := pin + 2*CONSENSUS_SPECS.SlotLength

	got := se.SaveBlockHeight(lastBlk, 200)
	assert.Equal(t, pin, got, "boundary case (exactly 2*SlotLength) should still pin")
}

func TestSaveBlockHeight_LastBlkBehindPinFallsThroughToClear(t *testing.T) {
	// Documents current behavior in a corner case that should be
	// unreachable in production. SaveBlockHeight is called from the
	// streamer with lastBlk = the block that was just processed, and
	// firstTxHeight is set to (block being processed - 1) by ExecuteBatch,
	// so the invariant lastBlk >= firstTxHeight holds whenever this is
	// called from the live indexer path.
	//
	// However, the implementation's clear branch fires for ANY non-pin
	// case — including lastBlk < pinHeight — so the state IS wiped here.
	// If a future change causes this to trigger in production it would
	// silently advance the checkpoint past pending tx output. Worth
	// pinning the behavior in a test.
	se := newSEWithTxOutput(100, 1)

	got := se.SaveBlockHeight(50, 200)
	assert.Equal(t, uint64(50), got)
	assert.Empty(t, se.TxOutput, "current behavior: clear fires for any non-pin case")
	assert.Equal(t, uint64(0), se.firstTxHeight)
}

func TestSaveBlockHeight_NoFirstTxHeightSkipsPinLogic(t *testing.T) {
	// TxOutput populated but firstTxHeight==0 — defensive case where
	// state is in an inconsistent intermediate. Should advance to lastBlk
	// without touching state.
	se := newSEWithTxOutput(0, 2)

	got := se.SaveBlockHeight(1000, 500)
	assert.Equal(t, uint64(1000), got)
	assert.Len(t, se.TxOutput, 2, "state must be untouched when firstTxHeight==0")
}
