package streamer_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Shorten halt-retry backoff for the tests. Production defaults are 2s/30s.
func init() {
	streamer.HaltRetryInitialBackoff = 50 * time.Millisecond
	streamer.HaltRetryMaxBackoff = 200 * time.Millisecond
}

// These tests pin the streamer's halt-retry contract:
//
//  1. On HaltError, the streamer invokes haltReset (if set), uses the
//     returned slot start to rewind lastProcessed, and re-creates the
//     listener so subsequent delivery starts at the slot start.
//  2. Without haltReset, the streamer falls back to retrying the failing
//     block — pre-existing behavior, preserved.
//  3. A permanent halt (haltCheck always fires) does NOT advance
//     lastProcessed past the failing block, regardless of how many cycles
//     fire. Operator escape hatch must be used to break out.
//
// The shared trick: a callsHaltDb mock wraps the existing block storage
// and records every (startBlock) the streamer passes to ListenToBlockUpdates.
// That gives us a deterministic record of rewind behavior.

// callsHaltDb tracks listener invocations and simulates a haltCheck that
// fires on a target block exactly K times then succeeds.
type callsHaltDb struct {
	*MockHiveBlockDb

	mu             sync.Mutex
	listenerStarts []uint64 // startBlock arg of each ListenToBlockUpdates call
}

func (c *callsHaltDb) ListenToBlockUpdates(ctx context.Context, startBlock uint64, listener func(block hive_blocks.HiveBlock, headHeight *uint64) error) (cancel context.CancelFunc, errChan <-chan error) {
	c.mu.Lock()
	c.listenerStarts = append(c.listenerStarts, startBlock)
	c.mu.Unlock()
	return c.MockHiveBlockDb.ListenToBlockUpdates(ctx, startBlock, listener)
}

func (c *callsHaltDb) startsCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.listenerStarts)
}

func (c *callsHaltDb) startsSnapshot() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint64, len(c.listenerStarts))
	copy(out, c.listenerStarts)
	return out
}

// seedRange seeds blocks [first, last] inclusive into the mock.
func seedRange(t *testing.T, db hive_blocks.HiveBlocks, first, last uint64) {
	for i := first; i <= last; i++ {
		block := hive_blocks.HiveBlock{
			BlockNumber: i,
			BlockID:     fmt.Sprintf("blk-%d", i),
			Timestamp:   "2024-01-01T00:00:00",
			MerkleRoot:  fmt.Sprintf("root-%d", i),
			Transactions: []hive_blocks.Tx{
				{TransactionID: fmt.Sprintf("tx-%d", i)},
			},
		}
		assert.NoError(t, db.StoreBlocks(last, block))
	}
}

func TestHaltRetry_RewindsToSlotStart(t *testing.T) {
	// Slot 100..119, propose_block lands at block 110. K=2 halts then success.
	const slotStart = uint64(100)
	const haltBlock = uint64(110)
	const lastBlock = uint64(115)
	const K = 2

	db := &callsHaltDb{MockHiveBlockDb: &MockHiveBlockDb{}}
	test_utils.RunPlugin(t, db)
	seedRange(t, db, slotStart, lastBlock)

	var (
		processed       []uint64
		processedMu     sync.Mutex
		haltsRemaining  int32 = K
		haltResetCalls  int32
		haltSlotReturn  uint64 = slotStart
	)

	process := func(block hive_blocks.HiveBlock, headHeight *uint64) {
		processedMu.Lock()
		processed = append(processed, block.BlockNumber)
		processedMu.Unlock()
	}

	// haltCheck reports a halt error for `K` invocations on the haltBlock,
	// then no more. The streamer queries this AFTER process(); we set a
	// pending error inside process() based on the block number.
	var pendingHalt atomic.Pointer[error]
	haltCheck := func() error {
		ep := pendingHalt.Swap(nil)
		if ep == nil {
			return nil
		}
		return *ep
	}
	wrappedProcess := func(block hive_blocks.HiveBlock, headHeight *uint64) {
		process(block, headHeight)
		if block.BlockNumber == haltBlock && atomic.LoadInt32(&haltsRemaining) > 0 {
			atomic.AddInt32(&haltsRemaining, -1)
			err := errors.New("simulated unsafe fetch failure")
			pendingHalt.Store(&err)
		}
	}

	haltReset := func() (uint64, bool) {
		atomic.AddInt32(&haltResetCalls, 1)
		return haltSlotReturn, true
	}

	sr := streamer.NewStreamReader(db, wrappedProcess, nil, slotStart-1)
	sr.SetHaltCheck(haltCheck)
	sr.SetHaltReset(haltReset)

	agg := aggregate.New([]aggregate.Plugin{sr})
	test_utils.RunPlugin(t, agg)

	// Wait for at least K halts + 1 successful pass.
	assert.Eventually(t, func() bool {
		processedMu.Lock()
		defer processedMu.Unlock()
		// Count how many times haltBlock has been processed; we need K
		// halted attempts plus one successful (=K+1).
		count := 0
		for _, b := range processed {
			if b == haltBlock {
				count++
			}
		}
		return count >= K+1
	}, 5*time.Second, 50*time.Millisecond, "haltBlock should be re-processed K+1 times")

	// Give the streamer a tick to advance past haltBlock on the success cycle.
	time.Sleep(200 * time.Millisecond)

	assert.Equal(t, int32(K), atomic.LoadInt32(&haltResetCalls),
		"haltReset should fire exactly K times")

	// Listener was started K+1 times: one initial, K rewinds.
	starts := db.startsSnapshot()
	assert.GreaterOrEqual(t, len(starts), K+1, "listener must restart after each halt")
	// First listener: streamer init sets lastProcessed to startBlock-1=slotStart-2 (Init bound)
	// Subsequent listeners after a halt-with-rewind: startBlock = slotSlotReturn - 1 = slotStart - 1.
	// So entries [1..K] should all equal slotStart - 1.
	for i := 1; i <= K && i < len(starts); i++ {
		assert.Equal(t, slotStart-1, starts[i],
			"listener restart #%d after halt should rewind to slotStart-1=%d", i, slotStart-1)
	}
}

func TestHaltRetry_NoHaltResetFallsBackToRefeedFailingBlock(t *testing.T) {
	// Without SetHaltReset wired, halt-retry should fall back to re-feeding
	// only the failing block (lastProcessed unchanged across halt cycles).
	const haltBlock = uint64(110)
	const lastBlock = uint64(115)

	db := &callsHaltDb{MockHiveBlockDb: &MockHiveBlockDb{}}
	test_utils.RunPlugin(t, db)
	seedRange(t, db, 100, lastBlock)

	var (
		processedMu    sync.Mutex
		processed      []uint64
		haltsRemaining int32 = 2
	)

	var pendingHalt atomic.Pointer[error]
	haltCheck := func() error {
		ep := pendingHalt.Swap(nil)
		if ep == nil {
			return nil
		}
		return *ep
	}

	process := func(block hive_blocks.HiveBlock, headHeight *uint64) {
		processedMu.Lock()
		processed = append(processed, block.BlockNumber)
		processedMu.Unlock()
		if block.BlockNumber == haltBlock && atomic.LoadInt32(&haltsRemaining) > 0 {
			atomic.AddInt32(&haltsRemaining, -1)
			err := errors.New("simulated unsafe fetch failure")
			pendingHalt.Store(&err)
		}
	}

	sr := streamer.NewStreamReader(db, process, nil, 99)
	sr.SetHaltCheck(haltCheck)
	// NO SetHaltReset.

	agg := aggregate.New([]aggregate.Plugin{sr})
	test_utils.RunPlugin(t, agg)

	assert.Eventually(t, func() bool {
		processedMu.Lock()
		defer processedMu.Unlock()
		count := 0
		for _, b := range processed {
			if b == haltBlock {
				count++
			}
		}
		return count >= 3
	}, 5*time.Second, 50*time.Millisecond)

	time.Sleep(200 * time.Millisecond)

	// listenerStarts after the first should be haltBlock-1 (lastProcessed
	// is the last successfully processed block, which is haltBlock-1, since
	// the haltBlock failed). Without rewind, the streamer reuses lastProcessed.
	starts := db.startsSnapshot()
	assert.GreaterOrEqual(t, len(starts), 2, "at least one listener restart on halt")
	for i := 1; i < len(starts); i++ {
		assert.Equal(t, haltBlock-1, starts[i],
			"without SetHaltReset, listener restart should reuse lastProcessed=haltBlock-1=%d", haltBlock-1)
	}
}

func TestHaltRetry_PermanentHaltDoesNotAdvance(t *testing.T) {
	// When haltCheck always fires, the streamer must never advance past
	// the failing block, regardless of how many cycles fire. This is the
	// onboarding-from-stuck-CID scenario from the plan.
	const slotStart = uint64(100)
	const haltBlock = uint64(110)
	const lastBlock = uint64(112)

	db := &callsHaltDb{MockHiveBlockDb: &MockHiveBlockDb{}}
	test_utils.RunPlugin(t, db)
	seedRange(t, db, slotStart, lastBlock)

	var (
		processedMu sync.Mutex
		processed   []uint64
		haltCount   int32
	)

	var pendingHalt atomic.Pointer[error]
	haltCheck := func() error {
		ep := pendingHalt.Swap(nil)
		if ep == nil {
			return nil
		}
		return *ep
	}

	process := func(block hive_blocks.HiveBlock, headHeight *uint64) {
		processedMu.Lock()
		processed = append(processed, block.BlockNumber)
		processedMu.Unlock()
		if block.BlockNumber == haltBlock {
			atomic.AddInt32(&haltCount, 1)
			err := errors.New("permanently unreachable CID")
			pendingHalt.Store(&err)
		}
	}

	haltReset := func() (uint64, bool) { return slotStart, true }

	sr := streamer.NewStreamReader(db, process, nil, slotStart-1)
	sr.SetHaltCheck(haltCheck)
	sr.SetHaltReset(haltReset)

	agg := aggregate.New([]aggregate.Plugin{sr})
	test_utils.RunPlugin(t, agg)

	// Let several halt cycles run.
	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&haltCount) >= 3
	}, 10*time.Second, 100*time.Millisecond, "expect at least 3 halt cycles on permanent halt")

	// Verify nothing past haltBlock was ever processed.
	processedMu.Lock()
	defer processedMu.Unlock()
	for _, b := range processed {
		assert.LessOrEqual(t, b, haltBlock,
			"streamer must NOT advance past permanently-halting block; saw block %d", b)
	}
}
