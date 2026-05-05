package test_utils

import (
	"context"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/witnesses"
)

type MockWitnessDb struct {
	aggregate.Plugin
	ByAccount map[string]*witnesses.Witness
	ByPeerId  map[string][]witnesses.Witness
}

func (m *MockWitnessDb) StoreNodeAnnouncement(_ context.Context, _ string) error { return nil }
func (m *MockWitnessDb) SetWitnessUpdate(_ context.Context, _ witnesses.SetWitnessUpdateType) error {
	return nil
}
func (m *MockWitnessDb) GetLastestWitnesses(_ context.Context, _ ...witnesses.SearchOption) ([]witnesses.Witness, error) {
	return nil, nil
}
func (m *MockWitnessDb) GetWitnessesAtBlockHeight(_ context.Context, _ uint64, _ ...witnesses.SearchOption) ([]witnesses.Witness, error) {
	return nil, nil
}

func (m *MockWitnessDb) GetWitnessesByPeerId(_ context.Context, peerIds []string, _ ...witnesses.SearchOption) ([]witnesses.Witness, error) {
	if m.ByPeerId == nil {
		return nil, nil
	}
	for _, pid := range peerIds {
		if ws, ok := m.ByPeerId[pid]; ok {
			return ws, nil
		}
	}
	return nil, nil
}

func (m *MockWitnessDb) GetWitnessAtHeight(_ context.Context, account string, _ *uint64) (*witnesses.Witness, error) {
	if m.ByAccount == nil {
		return nil, nil
	}
	if w, ok := m.ByAccount[account]; ok {
		return w, nil
	}
	return nil, nil
}
