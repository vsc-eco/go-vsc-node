package test_utils

import (
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/witnesses"
)

type MockWitnessDb struct {
	aggregate.Plugin
	ByAccount map[string]*witnesses.Witness
	ByPeerId  map[string][]witnesses.Witness
}

func (m *MockWitnessDb) StoreNodeAnnouncement(string) error                { return nil }
func (m *MockWitnessDb) SetWitnessUpdate(witnesses.SetWitnessUpdateType) error { return nil }
func (m *MockWitnessDb) GetLastestWitnesses(...witnesses.SearchOption) ([]witnesses.Witness, error) {
	return nil, nil
}
func (m *MockWitnessDb) GetWitnessesAtBlockHeight(uint64, ...witnesses.SearchOption) ([]witnesses.Witness, error) {
	return nil, nil
}

func (m *MockWitnessDb) GetWitnessesByPeerId(peerIds []string, _ ...witnesses.SearchOption) ([]witnesses.Witness, error) {
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

func (m *MockWitnessDb) GetWitnessAtHeight(account string, _ *uint64) (*witnesses.Witness, error) {
	if m.ByAccount == nil {
		return nil, nil
	}
	if w, ok := m.ByAccount[account]; ok {
		return w, nil
	}
	return nil, nil
}
