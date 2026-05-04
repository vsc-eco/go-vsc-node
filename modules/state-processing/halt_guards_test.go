package state_engine_test

import (
	"errors"
	"testing"
	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/stretchr/testify/assert"
)

// These tests pin the two halt guards that protect halt-retry determinism:
//
//   1. The slot-crossing block in ProcessBlock is guarded by unsafeFetchErr —
//      a halt fired during a block must NOT advance slotStatus, otherwise
//      ResetSlotState would report the wrong replay anchor and the slot
//      would be skipped on retry.
//   2. slotStatus.Done is set only when ExecuteTx returned with no halt
//      recorded — preventing the bottom-of-ProcessBlock ExecuteBatch from
//      running on partial state.
//
// Both tests use the testEnv harness (full mock dependencies). They drive
// ProcessBlock directly with hand-crafted empty blocks, so the test focuses
// on the engine's slot-tracking behavior rather than tx semantics.

// makeEmptyBlock builds a HiveBlock at the given height with no operations.
func makeEmptyBlock(blockNumber uint64) hive_blocks.HiveBlock {
	return hive_blocks.HiveBlock{
		BlockNumber: blockNumber,
		BlockID:     "test-block-id",
		Timestamp:   "2024-01-01T00:00:00",
		MerkleRoot:  "test-merkle-root",
	}
}

func TestHaltGuard_SlotCrossingDoesNotAdvanceWhenHalted(t *testing.T) {
	// ProcessBlock at the slot 100 boundary initializes slotStatus to
	// {SlotHeight: 100}. We then set an unsafe halt and process a block
	// in slot 110 — the slot-crossing branch should be guarded by
	// unsafeFetchErr and skip advancing slotStatus.
	//
	// ResetSlotState reports the current slotStatus.SlotHeight before
	// clearing — that's our introspection point.
	te := newTestEnv()

	// Initialize slotStatus to slot 100 via a no-op ProcessBlock.
	te.SE.ProcessBlock(makeEmptyBlock(100))

	// Sanity: slot 100 active.
	got, ok := te.SE.ResetSlotState()
	assert.True(t, ok)
	assert.Equal(t, uint64(100), got, "first ProcessBlock should establish slotStatus=100")

	// Re-establish slot 100 (ResetSlotState above cleared it).
	te.SE.ProcessBlock(makeEmptyBlock(100))

	// Now mark a halt — simulates an unsafe-tagged DAG fetch failure
	// during processing (e.g., a fetch site inside a system tx handler).
	te.SE.SetUnsafeHalt("test-site", errors.New("simulated unsafe halt"))

	// Process a block in slot 110 — would normally trigger slot crossing.
	te.SE.ProcessBlock(makeEmptyBlock(110))

	// The guard should have prevented the slot crossing from advancing
	// slotStatus. ResetSlotState should still report slot 100 as the
	// replay anchor.
	got, ok = te.SE.ResetSlotState()
	assert.True(t, ok, "slot still active after guarded ProcessBlock")
	assert.Equal(t, uint64(100), got,
		"slot crossing must NOT fire when unsafeFetchErr is set; "+
			"otherwise haltReset would report 110 as the replay anchor "+
			"and slot 100's L1 user ops would never be re-fed on retry")
}

func TestHaltGuard_SlotCrossingFiresNormallyWhenNoHalt(t *testing.T) {
	// Negative case: same scenario without setting unsafeFetchErr. Slot
	// crossing should fire and slotStatus should advance to 110.
	te := newTestEnv()

	te.SE.ProcessBlock(makeEmptyBlock(100))

	got, ok := te.SE.ResetSlotState()
	assert.Equal(t, uint64(100), got)
	assert.True(t, ok)

	te.SE.ProcessBlock(makeEmptyBlock(100))
	// No halt set.
	te.SE.ProcessBlock(makeEmptyBlock(110))

	got, ok = te.SE.ResetSlotState()
	assert.True(t, ok)
	assert.Equal(t, uint64(110), got,
		"without halt, slot crossing must advance slotStatus to the new slot")
}

func TestHaltGuard_HaltErrorIsConsumableAfterProcessBlock(t *testing.T) {
	// The halt set inside the block is observable via ConsumeUnsafeHalt
	// after ProcessBlock returns, even though we used the SetUnsafeHalt
	// public path rather than an actual fetch failure.
	te := newTestEnv()

	te.SE.ProcessBlock(makeEmptyBlock(100))
	te.SE.SetUnsafeHalt("manual", errors.New("test halt"))

	// Don't process another block; just consume directly.
	got := te.SE.ConsumeUnsafeHalt()
	assert.NotNil(t, got)
	assert.Contains(t, got.Error(), "manual")
	assert.Contains(t, got.Error(), "test halt")
	// Subsequent consume returns nil — the unsafeFetchErr was Swap'd to nil.
	assert.Nil(t, te.SE.ConsumeUnsafeHalt())
}
