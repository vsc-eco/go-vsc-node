package test_utils

import contract_execution_context "vsc-node/modules/contract/execution-context"

type mockStateStore struct {
	store     map[string][]byte
	cache     map[string][]byte
	deletions map[string]bool
}

func NewInMemoryStateStore() contract_execution_context.StateStore {
	return &mockStateStore{
		store:     make(map[string][]byte),
		cache:     make(map[string][]byte),
		deletions: make(map[string]bool),
	}
}

// Delete implements contract_execution_context.StateStore.
func (m *mockStateStore) Delete(key string) {
	delete(m.cache, key)
	m.deletions[key] = true
}

// Get implements contract_execution_context.StateStore.
func (m *mockStateStore) Get(key string) []byte {
	if m.deletions[key] {
		return []byte{}
	} else if _, exist := m.cache[key]; !exist {
		if val, exist := m.store[key]; !exist {
			return []byte{}
		} else {
			m.cache[key] = val
		}
	}
	return m.cache[key]
}

// Set implements contract_execution_context.StateStore.
func (m *mockStateStore) Set(key string, value []byte) {
	m.cache[key] = value
	delete(m.deletions, key)
}

func (m *mockStateStore) Commit() {
	for key, val := range m.cache {
		if !m.deletions[key] {
			m.store[key] = val
		}
	}
	for key := range m.deletions {
		delete(m.store, key)
	}
	m.cache = make(map[string][]byte)
	m.deletions = make(map[string]bool)
}

func (m *mockStateStore) Rollback() {
	m.cache = make(map[string][]byte)
	m.deletions = make(map[string]bool)
}
