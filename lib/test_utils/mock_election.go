package test_utils

import (
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/elections"
)

type MockElectionDb struct {
	aggregate.Plugin
}

func (m *MockElectionDb) StoreElection(elecResult elections.ElectionResult) error {
	return nil
}

func (m *MockElectionDb) GetElection(epoch uint64) *elections.ElectionResult {
	return nil
}

func (m *MockElectionDb) GetElectionByHeight(height uint64) (elections.ElectionResult, error) {
	return elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{
			Epoch: 1,
		},
	}, nil
}
