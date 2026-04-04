package test_utils

import (
	"fmt"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/elections"
)

type MockElectionDb struct {
	aggregate.Plugin
	Elections         map[uint64]*elections.ElectionResult
	ElectionsByHeight map[uint64]elections.ElectionResult
	PreviousElections []elections.ElectionResult
}

func (m *MockElectionDb) StoreElection(elecResult elections.ElectionResult) error {
	return nil
}

func (m *MockElectionDb) GetElection(epoch uint64) *elections.ElectionResult {
	if m.Elections == nil {
		return nil
	}
	if r, ok := m.Elections[epoch]; ok {
		return r
	}
	return nil
}

func (m *MockElectionDb) GetPreviousElections(beforeEpoch uint64, limit int) []elections.ElectionResult {
	if m.PreviousElections == nil {
		return nil
	}
	var result []elections.ElectionResult
	for _, e := range m.PreviousElections {
		if e.Epoch < beforeEpoch {
			result = append(result, e)
			if len(result) >= limit {
				break
			}
		}
	}
	return result
}

func (m *MockElectionDb) GetElectionByHeight(height uint64) (elections.ElectionResult, error) {
	if m.ElectionsByHeight == nil {
		return elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{
				Epoch: 1,
			},
		}, nil
	}
	if r, ok := m.ElectionsByHeight[height]; ok {
		return r, nil
	}
	// Fall back to returning any entry (needed for MaxInt64-1 queries)
	for _, r := range m.ElectionsByHeight {
		return r, nil
	}
	return elections.ElectionResult{}, fmt.Errorf("no election at height %d", height)
}
