package state_engine_test

import (
	"testing"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSeed returns a deterministic [32]byte seed for testing.
func testSeed(fill byte) [32]byte {
	var seed [32]byte
	for i := range seed {
		seed[i] = fill
	}
	return seed
}

// testWitnesses returns a witness list of the given size with accounts named "witness-0", "witness-1", etc.
func testWitnesses(n int) []stateEngine.Witness {
	witnesses := make([]stateEngine.Witness, n)
	for i := 0; i < n; i++ {
		witnesses[i] = stateEngine.Witness{
			Account: "witness-" + string(rune('a'+i)),
			Key:     "did:key:test",
		}
	}
	return witnesses
}

const (
	slotLength     = 10
	scheduleLength = 1200
	slotsPerRound  = scheduleLength / slotLength // 120
)

// ---------------------------------------------------------------------------
// CalculateSlotInfo
// ---------------------------------------------------------------------------

func TestCalculateSlotInfo(t *testing.T) {
	tests := []struct {
		name        string
		blockHeight uint64
		wantStart   uint64
		wantEnd     uint64
	}{
		{"zero", 0, 0, 10},
		{"slot-aligned 10", 10, 10, 20},
		{"slot-aligned 7200", 7200, 7200, 7210},
		{"mid-slot 5", 5, 0, 10},
		{"mid-slot 17", 17, 10, 20},
		{"mid-slot 7205", 7205, 7200, 7210},
		{"last block in slot", 9, 0, 10},
		{"large height", 100_005, 100_000, 100_010},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := stateEngine.CalculateSlotInfo(tt.blockHeight)
			assert.Equal(t, tt.wantStart, info.StartHeight)
			assert.Equal(t, tt.wantEnd, info.EndHeight)
		})
	}
}

func TestCalculateSlotInfoSpanIsAlwaysSlotLength(t *testing.T) {
	for _, bh := range []uint64{0, 1, 9, 10, 11, 99, 100, 7199, 7200, 7201, 999_999} {
		info := stateEngine.CalculateSlotInfo(bh)
		assert.Equal(t, uint64(slotLength), info.EndHeight-info.StartHeight,
			"slot span should be SlotLength=%d at blockHeight=%d", slotLength, bh)
	}
}

// ---------------------------------------------------------------------------
// CalculateEpochRound
// ---------------------------------------------------------------------------

func TestCalculateEpochRound(t *testing.T) {
	modLength := uint64(scheduleLength) * 7200 // 8,640,000

	tests := []struct {
		name     string
		bh       uint64
		wantPast uint64
		wantNext uint64
	}{
		{"zero", 0, 0, modLength},
		{"mid-epoch", 5000, 0, modLength},
		{"exactly on boundary", modLength, modLength, 2 * modLength},
		{"one past boundary", modLength + 1, modLength, 2 * modLength},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := stateEngine.CalculateEpochRound(tt.bh)
			assert.Equal(t, tt.wantPast, info.PastRoundHeight)
			assert.Equal(t, tt.wantNext, info.NextRoundHeight)
		})
	}
}

func TestCalculateEpochRoundPastPlusNextEqualsDoubleModLength(t *testing.T) {
	// For any height, PastRoundHeight + (NextRoundHeight - PastRoundHeight) == modLength
	modLength := uint64(scheduleLength) * 7200
	for _, bh := range []uint64{0, 1, 1199, 1200, 7200, modLength - 1, modLength, modLength + 1} {
		info := stateEngine.CalculateEpochRound(bh)
		span := info.NextRoundHeight - info.PastRoundHeight
		assert.Equal(t, modLength, span,
			"epoch span should equal modLength at blockHeight=%d", bh)
	}
}

// ---------------------------------------------------------------------------
// GenerateSchedule
// ---------------------------------------------------------------------------

func TestGenerateScheduleSlotCount(t *testing.T) {
	witnesses := testWitnesses(13)
	seed := testSeed(0xAB)
	schedule := stateEngine.GenerateSchedule(0, witnesses, seed)
	assert.Len(t, schedule, slotsPerRound, "schedule should have exactly 120 slots")
}

func TestGenerateScheduleSlotHeights(t *testing.T) {
	witnesses := testWitnesses(10)
	seed := testSeed(0x01)

	// Test at block 0 (round starts at 0)
	schedule := stateEngine.GenerateSchedule(0, witnesses, seed)
	for i, slot := range schedule {
		expected := uint64(i) * slotLength
		assert.Equal(t, expected, slot.SlotHeight,
			"slot %d should have SlotHeight=%d", i, expected)
	}

	// Test at block 1500 (round starts at 1200)
	schedule = stateEngine.GenerateSchedule(1500, witnesses, seed)
	for i, slot := range schedule {
		expected := uint64(i)*slotLength + 1200
		assert.Equal(t, expected, slot.SlotHeight,
			"slot %d should have SlotHeight=%d", i, expected)
	}
}

func TestGenerateScheduleContainsAllWitnesses(t *testing.T) {
	witnesses := testWitnesses(13)
	seed := testSeed(0x42)
	schedule := stateEngine.GenerateSchedule(0, witnesses, seed)

	seen := make(map[string]bool)
	for _, slot := range schedule {
		seen[slot.Account] = true
	}
	for _, w := range witnesses {
		assert.True(t, seen[w.Account],
			"witness %s should appear in schedule", w.Account)
	}
}

func TestGenerateScheduleFairDistribution(t *testing.T) {
	witnesses := testWitnesses(13)
	seed := testSeed(0xFF)
	schedule := stateEngine.GenerateSchedule(0, witnesses, seed)

	counts := make(map[string]int)
	for _, slot := range schedule {
		counts[slot.Account]++
	}

	// With 120 slots and 13 witnesses: 120/13 = 9 remainder 3
	// So 3 witnesses get 10 slots, 10 witnesses get 9 slots (round-robin before shuffle)
	minExpected := slotsPerRound / len(witnesses)         // 9
	maxExpected := slotsPerRound/len(witnesses) + 1       // 10

	for account, count := range counts {
		assert.GreaterOrEqual(t, count, minExpected,
			"%s should have at least %d slots", account, minExpected)
		assert.LessOrEqual(t, count, maxExpected,
			"%s should have at most %d slots", account, maxExpected)
	}
}

func TestGenerateScheduleDeterministic(t *testing.T) {
	witnesses := testWitnesses(13)
	seed := testSeed(0xDE)

	s1 := stateEngine.GenerateSchedule(0, witnesses, seed)
	s2 := stateEngine.GenerateSchedule(0, witnesses, seed)

	require.Len(t, s1, len(s2))
	for i := range s1 {
		assert.Equal(t, s1[i].Account, s2[i].Account,
			"slot %d account should be identical across calls", i)
		assert.Equal(t, s1[i].SlotHeight, s2[i].SlotHeight,
			"slot %d height should be identical across calls", i)
	}
}

func TestGenerateScheduleDifferentSeedsDifferentOrder(t *testing.T) {
	witnesses := testWitnesses(13)
	seedA := testSeed(0x01)
	seedB := testSeed(0x02)

	sA := stateEngine.GenerateSchedule(0, witnesses, seedA)
	sB := stateEngine.GenerateSchedule(0, witnesses, seedB)

	differ := false
	for i := range sA {
		if sA[i].Account != sB[i].Account {
			differ = true
			break
		}
	}
	assert.True(t, differ, "different seeds should produce different orderings")
}

func TestGenerateScheduleSingleWitness(t *testing.T) {
	witnesses := testWitnesses(1)
	seed := testSeed(0x00)
	schedule := stateEngine.GenerateSchedule(0, witnesses, seed)

	assert.Len(t, schedule, slotsPerRound)
	for _, slot := range schedule {
		assert.Equal(t, witnesses[0].Account, slot.Account,
			"single witness should fill all slots")
	}
}

// ---------------------------------------------------------------------------
// CalculateSlotLeader
// ---------------------------------------------------------------------------

func TestCalculateSlotLeaderReturnsValidSlot(t *testing.T) {
	witnesses := testWitnesses(13)
	seed := testSeed(0xAA)

	leader := stateEngine.CalculateSlotLeader(0, witnesses, seed)
	require.NotNil(t, leader, "slot leader should not be nil")

	// SlotHeight should match the slot start for block 0
	slotInfo := stateEngine.CalculateSlotInfo(0)
	assert.Equal(t, uint64(slotInfo.StartHeight), leader.SlotHeight)

	// Account should be one of the witnesses
	found := false
	for _, w := range witnesses {
		if w.Account == leader.Account {
			found = true
			break
		}
	}
	assert.True(t, found, "leader account should be in witness list")
}

func TestCalculateSlotLeaderDeterministic(t *testing.T) {
	witnesses := testWitnesses(13)
	seed := testSeed(0xBB)

	l1 := stateEngine.CalculateSlotLeader(25, witnesses, seed)
	l2 := stateEngine.CalculateSlotLeader(25, witnesses, seed)
	require.NotNil(t, l1)
	require.NotNil(t, l2)
	assert.Equal(t, l1.Account, l2.Account)
	assert.Equal(t, l1.SlotHeight, l2.SlotHeight)
}

func TestCalculateSlotLeaderSameSlotSameLeader(t *testing.T) {
	witnesses := testWitnesses(13)
	seed := testSeed(0xCC)

	// Blocks 20 and 25 are in the same slot (start=20)
	l20 := stateEngine.CalculateSlotLeader(20, witnesses, seed)
	l25 := stateEngine.CalculateSlotLeader(25, witnesses, seed)
	require.NotNil(t, l20)
	require.NotNil(t, l25)
	assert.Equal(t, l20.Account, l25.Account,
		"same slot should have same leader")
}

func TestCalculateSlotLeaderDifferentSlotsCanDiffer(t *testing.T) {
	witnesses := testWitnesses(13)
	seed := testSeed(0xDD)

	// Collect leaders across all 120 slots in round starting at 0
	leaders := make(map[string]bool)
	for slot := uint64(0); slot < slotsPerRound; slot++ {
		bh := slot * slotLength
		leader := stateEngine.CalculateSlotLeader(bh, witnesses, seed)
		require.NotNil(t, leader, "leader should not be nil at bh=%d", bh)
		leaders[leader.Account] = true
	}
	// With 13 witnesses and 120 slots, all witnesses should appear as leaders
	assert.Equal(t, len(witnesses), len(leaders),
		"all witnesses should lead at least one slot across the full round")
}

func TestCalculateSlotLeaderMidSlotHeight(t *testing.T) {
	witnesses := testWitnesses(13)
	seed := testSeed(0xEE)

	// Block 1207 is mid-slot in round starting at 1200, slot start = 1200
	leader := stateEngine.CalculateSlotLeader(1207, witnesses, seed)
	require.NotNil(t, leader)
	assert.Equal(t, uint64(1200), leader.SlotHeight,
		"leader SlotHeight should be the slot start, not the query height")
}
