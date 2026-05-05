package test_utils

import (
	"context"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/nonces"
)

type MockNonceDb struct {
	aggregate.Plugin
	Nonces map[string]uint64
}

func (m *MockNonceDb) GetNonce(_ context.Context, account string) (nonces.NonceRecord, error) {
	n, exists := m.Nonces[account]
	if !exists {
		return nonces.NonceRecord{Account: account, Nonce: 0}, nil
	}
	return nonces.NonceRecord{Account: account, Nonce: n}, nil
}

func (m *MockNonceDb) SetNonce(_ context.Context, account string, nonce uint64) error {
	m.Nonces[account] = nonce
	return nil
}
