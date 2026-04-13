package test_utils

import (
	"context"
	"sync"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/consensus_state"

	"github.com/chebyrash/promise"
)

const mockConsensusSingletonID = "singleton"

// MockConsensusState is an in-memory implementation of consensus_state.ConsensusState for tests.
type MockConsensusState struct {
	aggregate.Plugin
	mu sync.Mutex
	S  consensus_state.ChainConsensusState
}

func NewMockConsensusState() *MockConsensusState {
	return &MockConsensusState{
		S: consensus_state.ChainConsensusState{
			ID: mockConsensusSingletonID,
		},
	}
}

var _ consensus_state.ConsensusState = (*MockConsensusState)(nil)

func (m *MockConsensusState) Init() error { return nil }

func (m *MockConsensusState) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		resolve(nil)
	})
}

func (m *MockConsensusState) Stop() error { return nil }

func (m *MockConsensusState) Get(_ context.Context) (consensus_state.ChainConsensusState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.S.ID == "" {
		m.S.ID = mockConsensusSingletonID
	}
	return m.S, nil
}

func (m *MockConsensusState) Upsert(_ context.Context, state consensus_state.ChainConsensusState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.S = state
	if m.S.ID == "" {
		m.S.ID = mockConsensusSingletonID
	}
	return nil
}

func (m *MockConsensusState) SetPendingProposal(_ context.Context, p *consensus_state.PendingConsensusProposal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.S.PendingProposal = p
	return nil
}

func (m *MockConsensusState) ClearPendingProposal(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.S.PendingProposal = nil
	return nil
}

func (m *MockConsensusState) SetAdoptedVersion(_ context.Context, v consensusversion.Version) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.S.AdoptedVersion = v
	return nil
}

func (m *MockConsensusState) SetProcessingSuspended(_ context.Context, suspended bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.S.ProcessingSuspended = suspended
	return nil
}

func (m *MockConsensusState) SetMinRequiredAndClearSuspension(_ context.Context, v consensusversion.Version) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.S.MinRequiredVersion = &v
	m.S.ProcessingSuspended = false
	m.S.AdoptedVersion = v
	m.S.PendingProposal = nil
	return nil
}

func (m *MockConsensusState) SetNextActivation(_ context.Context, a *consensus_state.ConsensusActivation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.S.NextActivation = a
	return nil
}

func (m *MockConsensusState) ClearNextActivation(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.S.NextActivation = nil
	return nil
}

// Snapshot returns a copy of current state (for assertions).
func (m *MockConsensusState) Snapshot() consensus_state.ChainConsensusState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.S
}

// ReplaceState overwrites the full document (for test setup).
func (m *MockConsensusState) ReplaceState(st consensus_state.ChainConsensusState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.S = st
	if m.S.ID == "" {
		m.S.ID = mockConsensusSingletonID
	}
}
