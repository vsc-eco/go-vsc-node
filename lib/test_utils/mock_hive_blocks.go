package test_utils

import (
	"context"
	"fmt"

	"vsc-node/lib/utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/chebyrash/promise"
)

type MockHiveBlockDb struct {
	aggregate.Plugin
	Blocks             []hive_blocks.HiveBlock
	LastProcessedBlock uint64
	HeadHeight         uint64
	Metadata           hive_blocks.Document
}

var _ hive_blocks.HiveBlocks = &MockHiveBlockDb{}

// StoreBlocks implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) StoreBlocks(headBlock uint64, blocks ...hive_blocks.HiveBlock) error {
	m.Blocks = append(m.Blocks, blocks...)
	m.HeadHeight = headBlock
	return nil
}

// ClearBlocks implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) ClearBlocks() error {
	m.Blocks = nil
	m.LastProcessedBlock = 0
	return nil
}

// StoreLastProcessedBlock implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) StoreLastProcessedBlock(blockNumber uint64) error {
	m.LastProcessedBlock = blockNumber
	return nil
}

// GetLastProcessedBlock implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) GetLastProcessedBlock() (uint64, error) {
	if m.Blocks == nil {
		return 0, nil
	}
	return m.LastProcessedBlock, nil
}

// FetchStoredBlocks implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) FetchStoredBlocks(startBlock, endBlock uint64) ([]hive_blocks.HiveBlock, error) {
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
func (m *MockHiveBlockDb) ListenToBlockUpdates(
	ctx context.Context,
	startBlock uint64,
	listener func(block hive_blocks.HiveBlock, headHeight *uint64) error,
) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(ctx)
	errChan := make(chan error)
	if m.Blocks == nil {
		return cancel, errChan
	}

	go func() {
		for _, block := range m.Blocks {
			if block.BlockNumber >= startBlock {
				select {
				case <-ctx.Done():
					break
				default:
					err := listener(block, &m.HeadHeight)
					if err != nil {
						errChan <- err
						break
					}
				}
			}
		}
	}()

	return cancel, errChan
}

// GetHighestBlock implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) GetHighestBlock() (uint64, error) {
	if m.Blocks == nil || len(m.Blocks) == 0 {
		return m.HeadHeight, nil
	}
	return m.Blocks[len(m.Blocks)-1].BlockNumber, nil
}

// GetBlock implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) GetBlock(blockNum uint64) (hive_blocks.HiveBlock, error) {
	for _, block := range m.Blocks {
		if block.BlockNumber == blockNum {
			return block, nil
		}
	}
	return hive_blocks.HiveBlock{}, fmt.Errorf("block not found")
}

// GetMetadata implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) GetMetadata() (hive_blocks.Document, error) {
	return m.Metadata, nil
}

// SetMetadata implements hive_blocks.HiveBlocks.
func (m *MockHiveBlockDb) SetMetadata(doc hive_blocks.Document) error {
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
	m.Blocks = nil
	m.LastProcessedBlock = 0
	return nil
}
