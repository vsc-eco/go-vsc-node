package e2e

import (
	"context"
	"errors"
	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/chebyrash/promise"
)

type MockHiveDbs struct {
	blocks map[uint64]*hive_blocks.HiveBlock
}

func (m *MockHiveDbs) StoreBlocks(blocks ...hive_blocks.HiveBlock) error {
	for _, block := range blocks {
		m.blocks[block.BlockNumber] = &block
	}
	return nil
}

func (m *MockHiveDbs) ClearBlocks() error {
	return nil
}

func (m *MockHiveDbs) StoreLastProcessedBlock(blockNumber uint64) error {
	return nil
}

func (m *MockHiveDbs) GetLastProcessedBlock() (uint64, error) {
	return 0, nil
}

func (m *MockHiveDbs) FetchStoredBlocks(startBlock uint64, endBlock uint64) ([]hive_blocks.HiveBlock, error) {
	return nil, nil
}

func (m *MockHiveDbs) ListenToBlockUpdates(ctx context.Context, startBlock uint64, listener func(block hive_blocks.HiveBlock) error) (context.CancelFunc, <-chan error) {
	return nil, nil
}

func (m *MockHiveDbs) GetHighestBlock() (uint64, error) {
	return 0, nil
}

func (m *MockHiveDbs) GetBlock(blockNum uint64) (hive_blocks.HiveBlock, error) {
	if m.blocks[blockNum] == nil {
		return hive_blocks.HiveBlock{}, errors.New("block not found")
	}
	return *m.blocks[blockNum], nil
}

func (m *MockHiveDbs) Init() error {
	m.blocks = make(map[uint64]*hive_blocks.HiveBlock)
	return nil
}

func (m *MockHiveDbs) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(interface{}), reject func(error)) {
		resolve(nil)
	})
}

func (m *MockHiveDbs) Stop() error {
	return nil
}

var _ hive_blocks.HiveBlocks = &MockHiveDbs{}
