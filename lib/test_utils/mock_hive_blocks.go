package test_utils

import (
	"context"
	"fmt"
	"sync"
	"time"

	"vsc-node/lib/utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/chebyrash/promise"
)

// MockBlockPollInterval is how often the mock's block feed re-scans for
// newly stored blocks. Short enough to keep tests fast.
var MockBlockPollInterval = 20 * time.Millisecond

// MockHiveBlockDb is an in-memory hive_blocks.HiveBlocks. All accessors are
// goroutine-safe: the block feed (ListenToBlockUpdates) runs concurrently
// with producers (StoreBlocks via the Streamer) and test assertions.
type MockHiveBlockDb struct {
	aggregate.Plugin
	Blocks             []hive_blocks.HiveBlock
	LastProcessedBlock uint64
	HeadHeight         uint64
	Metadata           hive_blocks.Document

	mtx sync.Mutex
}

var _ hive_blocks.HiveBlocks = &MockHiveBlockDb{}

// StoreBlocks implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) StoreBlocks(headBlock uint64, blocks ...hive_blocks.HiveBlock) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.Blocks = append(m.Blocks, blocks...)
	m.HeadHeight = headBlock
	return nil
}

// ClearBlocks implements hive_blocks.HiveBlocks.
//
// Matches the production store, whose ClearBlocks deletes every document —
// blocks AND metadata — so the head height resets too and a restarted
// streamer falls back to its default start block.
func (m *MockHiveBlockDb) ClearBlocks() error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.Blocks = nil
	m.LastProcessedBlock = 0
	m.HeadHeight = 0
	return nil
}

// StoreLastProcessedBlock implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) StoreLastProcessedBlock(blockNumber uint64) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.LastProcessedBlock = blockNumber
	return nil
}

// GetLastProcessedBlock implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) GetLastProcessedBlock() (uint64, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	if m.Blocks == nil {
		return 0, nil
	}
	return m.LastProcessedBlock, nil
}

// FetchStoredBlocks implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) FetchStoredBlocks(startBlock, endBlock uint64) ([]hive_blocks.HiveBlock, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	if m.Blocks == nil {
		return []hive_blocks.HiveBlock{}, nil
	}

	startIndex := len(m.Blocks)
	endIndex := len(m.Blocks)
	for i, block := range m.Blocks {
		if block.BlockNumber == startBlock {
			startIndex = i
		}
		if block.BlockNumber == endBlock {
			endIndex = i
			break
		}
	}

	return m.Blocks[startIndex : endIndex+1], nil
}

// ListenToBlockUpdates implements hive_blocks.HiveBlocks.
//
// Mirrors the production feed: a re-polling loop that delivers every stored
// block above the cursor in order (so blocks stored after the listener
// starts are picked up), advancing the cursor as it delivers (no
// re-delivery).
//
// Mirrors the production shutdown contract too: the returned CancelFunc
// signals the feed to stop AND waits for the loop goroutine to exit, so a
// completed cancel guarantees the in-flight listener call (the current
// block's processing) has returned and no new one can start.
func (m *MockHiveBlockDb) ListenToBlockUpdates(
	ctx context.Context,
	startBlock uint64,
	listener func(block hive_blocks.HiveBlock, headHeight *uint64) error,
) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(ctx)
	errChan := make(chan error)
	done := make(chan struct{})
	fail := func(err error) {
		select {
		case errChan <- err:
		case <-ctx.Done():
		}
	}

	go func() {
		// close(done) runs after the recover defer below (defers are LIFO).
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				fail(fmt.Errorf("panic in mock block listener at block %d: %v", startBlock, r))
			}
		}()
		// next is the lowest block number not yet delivered.
		next := startBlock
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// Snapshot under the lock; the listener call itself must run
			// without it (the listener may call back into the mock).
			m.mtx.Lock()
			blocks := m.Blocks
			headHeight := m.HeadHeight
			m.mtx.Unlock()
			for _, block := range blocks {
				if block.BlockNumber < next {
					continue
				}
				select {
				case <-ctx.Done():
					return
				default:
				}
				if err := listener(block, &headHeight); err != nil {
					fail(err)
					return
				}
				next = block.BlockNumber + 1
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(MockBlockPollInterval):
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}, errChan
}

// GetHighestBlock implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) GetHighestBlock() (uint64, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	if m.Blocks == nil || len(m.Blocks) == 0 {
		return m.HeadHeight, nil
	}
	return m.Blocks[len(m.Blocks)-1].BlockNumber, nil
}

// GetBlock implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) GetBlock(blockNum uint64) (hive_blocks.HiveBlock, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	for _, block := range m.Blocks {
		if block.BlockNumber == blockNum {
			return block, nil
		}
	}
	return hive_blocks.HiveBlock{}, fmt.Errorf("block not found")
}

// GetMetadata implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) GetMetadata() (hive_blocks.Document, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	return m.Metadata, nil
}

// SetMetadata implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) SetMetadata(doc hive_blocks.Document) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.Metadata = doc
	return nil
}

// Init implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) Init() error {
	return nil
}

// Start implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

// Stop implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) Stop() error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.Blocks = nil
	m.LastProcessedBlock = 0
	return nil
}
