package state_engine

import (
	"testing"
	contract_session "vsc-node/modules/contract/session"

	"github.com/stretchr/testify/assert"
)

// SaveBlockHeight is small enough that direct construction beats the full
// testEnv harness — we just need TxOutput / firstTxHeight populated.

func newSEWithTxOutput(firstTxHeight uint64, n int) *StateEngine {
	se := &StateEngine{
		TxOutput:        make(map[string]TxOutput),
		ContractResults: make(map[string][]ContractResult),
		TempOutputs:     make(map[string]*contract_session.TempOutput),
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
	// firstTxHeight=100, lastBlk well within the window of pinHeight (=99).
	se := newSEWithTxOutput(100, 1)

	pin := uint64(99) // firstTxHeight - 1
	// One slot in: trivially within the pinWindowSlots-slot window.
	lastBlk := pin + CONSENSUS_SPECS.SlotLength

	got := se.SaveBlockHeight(lastBlk, 200)
	assert.Equal(t, pin, got, "should pin to firstTxHeight-1 within the window")

	// State should NOT be mutated while pinning.
	assert.Equal(t, uint64(100), se.firstTxHeight)
	assert.Len(t, se.TxOutput, 1)
}

func TestSaveBlockHeight_PinsThroughHealthyChurn(t *testing.T) {
	// 4 slots in (~120s) — inside the worst-case healthy producer latency
	// the pin window is sized for. Catches regressions that would shrink
	// the window back below normal-load expectations.
	se := newSEWithTxOutput(100, 1)
	pin := uint64(99)
	lastBlk := pin + 4*CONSENSUS_SPECS.SlotLength

	got := se.SaveBlockHeight(lastBlk, 200)
	assert.Equal(t, pin, got, "healthy 4-slot churn must stay pinned, not Flush")

	// Confirm no Flush fired.
	assert.Equal(t, uint64(100), se.firstTxHeight, "firstTxHeight must be intact")
	assert.Len(t, se.TxOutput, 1, "TxOutput must be intact within healthy window")
}

func TestSaveBlockHeight_PinExpiresAndFlushes(t *testing.T) {
	// One block past the window — must Flush.
	se := newSEWithTxOutput(100, 3)
	se.ContractResults["c"] = []ContractResult{{}}
	se.TempOutputs["c"] = &contract_session.TempOutput{}
	se.TxOutIds = []string{"tx-stale"}

	pin := uint64(99)
	lastBlk := pin + pinWindowSlots*CONSENSUS_SPECS.SlotLength + 1

	got := se.SaveBlockHeight(lastBlk, 200)
	assert.Equal(t, lastBlk, got, "past-window should advance to lastBlk")

	// Full Flush: leaving these populated would let the next batch's
	// callSession read ghost contract state from a slot whose
	// ContractOutput will never land.
	assert.Empty(t, se.TxOutput, "TxOutput must be cleared on pin expiry")
	assert.Equal(t, uint64(0), se.firstTxHeight, "firstTxHeight must be cleared on pin expiry")
	assert.Empty(t, se.TxOutIds, "TxOutIds must be cleared — keeping them feeds a stale MakeOplog")
	assert.Empty(t, se.ContractResults, "ContractResults must be cleared — ghost cross-batch state otherwise")
	assert.Empty(t, se.TempOutputs, "TempOutputs must be cleared — callSession would otherwise read stale entries")
}

func TestSaveBlockHeight_PinExactlyAtBoundary(t *testing.T) {
	// lastBlk - pinHeight == window : still within (the comparison is <=)
	se := newSEWithTxOutput(100, 1)
	pin := uint64(99)
	lastBlk := pin + pinWindowSlots*CONSENSUS_SPECS.SlotLength

	got := se.SaveBlockHeight(lastBlk, 200)
	assert.Equal(t, pin, got, "boundary case (exactly pinWindow) should still pin")
}

func TestSaveBlockHeight_LastBlkBehindPinFallsThroughToFlush(t *testing.T) {
	// Documents current behavior in a corner case that should be
	// unreachable in production. SaveBlockHeight is called from the
	// streamer with lastBlk = the block that was just processed, and
	// firstTxHeight is set to (block being processed - 1) by ExecuteBatch,
	// so the invariant lastBlk >= firstTxHeight holds whenever this is
	// called from the live indexer path.
	//
	// However, the implementation's Flush branch fires for ANY non-pin
	// case — including lastBlk < pinHeight — so the state IS wiped here.
	// If a future change causes this to trigger in production it would
	// silently advance the checkpoint past pending tx output. Worth
	// pinning the behavior in a test.
	se := newSEWithTxOutput(100, 1)

	got := se.SaveBlockHeight(50, 200)
	assert.Equal(t, uint64(50), got)
	assert.Empty(t, se.TxOutput, "current behavior: Flush fires for any non-pin case")
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

func TestSaveBlockHeight_SlotBoundaryAdvancesToLastBlk(t *testing.T) {
	// Pins the design decision documented above SaveBlockHeight: when a
	// producer's vsc.propose_block lands on L1, every ContractOutput
	// inside it has run Ingest -> Flush, so TxOutput is empty by the
	// time SaveBlockHeight runs. We must fall through to `return lastBlk`
	// — NOT consult vscBlocks and return SlotHeight+1 like the old
	// develop-era logic did. SlotHeight+1 is strictly behind lastBlk in
	// this scenario; consulting vscBlocks would force the indexer to
	// re-execute (SlotHeight, lastBlk] on crash recovery for blocks
	// that were already fully processed.
	//
	// If a future change re-introduces vscBlocks consultation here, this
	// test fails by returning a value other than lastBlk.

	// Post-Flush state: empty TxOutput, firstTxHeight=0.
	se := newSEWithTxOutput(0, 0)

	// Slot 100 ends at block 109; producer's L2 block typically lands
	// a few blocks into the next slot. Pick lastBlk=113 — old logic
	// would have returned SlotHeight+1 (=110 or so depending on
	// vscBlocks lookup), new logic returns 113.
	const lastBlk uint64 = 113

	got := se.SaveBlockHeight(lastBlk, 100)
	assert.Equal(t, lastBlk, got,
		"slot-boundary fall-through must return lastBlk verbatim — re-introducing vscBlocks lookup here would silently force unnecessary replay")
	assert.Empty(t, se.TxOutput, "state untouched in fall-through path")
	assert.Equal(t, uint64(0), se.firstTxHeight)
}
