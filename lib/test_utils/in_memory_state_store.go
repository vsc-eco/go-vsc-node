package test_utils

import contract_execution_context "vsc-node/modules/contract/execution-context"

type mockStateStore map[string][]byte

func NewInMemoryStateStore() contract_execution_context.StateStore {
	return &mockStateStore{}
}

// Delete implements contract_execution_context.StateStore.
func (m *mockStateStore) Delete(key string) {
	delete(*m, key)
}

// Get implements contract_execution_context.StateStore.
func (m *mockStateStore) Get(key string) []byte {
	return (*m)[key]
}

// Set implements contract_execution_context.StateStore.
func (m *mockStateStore) Set(key string, value []byte) {
	(*m)[key] = value
}
