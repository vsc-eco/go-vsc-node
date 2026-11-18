package test_utils

import (
	"fmt"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/elections"

	"go.mongodb.org/mongo-driver/mongo"
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

// GetElectionStrict mirrors the production strict variant: a genuine absence
// returns mongo.ErrNoDocuments (deterministic) so the #11 bond-lock fail-stop path
// can tell absence from a transient error.
func (m *MockElectionDb) GetElectionStrict(epoch uint64) (*elections.ElectionResult, error) {
	if m.Elections != nil {
		if r, ok := m.Elections[epoch]; ok {
			return r, nil
		}
	}
	return nil, mongo.ErrNoDocuments
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
	// A height that is not a key: the election at the greatest key below it, as
	// the real lookup answers (the election in force at that height), which also
	// serves the MaxInt64-1 "latest" queries. With no key below it, the lowest
	// key, so a map with entries always answers. Never "any entry": map order
	// would pick a different election from run to run.
	var below, lowest uint64
	haveBelow, haveLowest := false, false
	for h := range m.ElectionsByHeight {
		if h < height && (!haveBelow || h > below) {
			below, haveBelow = h, true
		}
		if !haveLowest || h < lowest {
			lowest, haveLowest = h, true
		}
	}
	if haveBelow {
		return m.ElectionsByHeight[below], nil
	}
	if haveLowest {
		return m.ElectionsByHeight[lowest], nil
	}
	// Match the real elections DB contract: a not-found lookup returns
	// mongo.ErrNoDocuments (wrapped for a readable message). Fail-stop
	// callers (GetElectionInfoOrBlock) rely on errors.Is to tell a
	// deterministic absence apart from an infra error they must block on.
	return elections.ElectionResult{}, fmt.Errorf("no election at height %d: %w", height, mongo.ErrNoDocuments)
}
