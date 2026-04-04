package test_utils

import (
	"fmt"
	"vsc-node/modules/aggregate"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
)

type MockVscBlocksDb struct {
	aggregate.Plugin
	Blocks []vscBlocks.VscHeaderRecord
}

func (m *MockVscBlocksDb) StoreHeader(header vscBlocks.VscHeaderRecord) {
	m.Blocks = append(m.Blocks, header)
}

func (m *MockVscBlocksDb) GetBlockByHeight(height uint64) (*vscBlocks.VscHeaderRecord, error) {
	for _, b := range m.Blocks {
		if uint64(b.SlotHeight) == height {
			return &b, nil
		}
	}
	return nil, fmt.Errorf("block not found at height %d", height)
}

func (m *MockVscBlocksDb) GetBlockById(id string) (*vscBlocks.VscHeaderRecord, error) {
	for _, b := range m.Blocks {
		if b.Id == id {
			return &b, nil
		}
	}
	return nil, fmt.Errorf("block not found: %s", id)
}

func (m *MockVscBlocksDb) GetBlocksByElection(epoch uint64) ([]vscBlocks.VscHeaderRecord, error) {
	var results []vscBlocks.VscHeaderRecord
	for _, b := range m.Blocks {
		if b.Epoch == epoch {
			results = append(results, b)
		}
	}
	return results, nil
}
