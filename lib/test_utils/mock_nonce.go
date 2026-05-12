package test_utils

import (
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/nonces"
)

type MockNonceDb struct {
	aggregate.Plugin
	Nonces map[string]uint64
}

func (m *MockNonceDb) GetNonce(account string) (nonces.NonceRecord, error) {
	n, exists := m.Nonces[account]
	if !exists {
		return nonces.NonceRecord{Account: account, Nonce: 0}, nil
	}
	return nonces.NonceRecord{Account: account, Nonce: n}, nil
}

func (m *MockNonceDb) SetNonce(account string, nonce uint64) error {
	m.Nonces[account] = nonce
	return nil
}

func (m *MockNonceDb) BulkSetNonces(updates map[string]uint64) error {
	for account, nonce := range updates {
		m.Nonces[account] = nonce
	}
	return nil
}
