package test_utils

import (
	"fmt"
	"slices"
	"vsc-node/modules/aggregate"
	tss "vsc-node/modules/db/vsc/tss"
)

// MockTssKeysDb implements tss.TssKeys interface for testing
type MockTssKeysDb struct {
	aggregate.Plugin
	Keys map[string]tss.TssKey
}

func (m *MockTssKeysDb) InsertKey(id string, t tss.TssKeyAlgo) error {
	if _, exists := m.Keys[id]; exists {
		return fmt.Errorf("key already exists")
	}
	m.Keys[id] = tss.TssKey{
		Id:   id,
		Algo: t,
	}
	return nil
}

func (m *MockTssKeysDb) FindKey(id string) (tss.TssKey, error) {
	key, exists := m.Keys[id]
	if !exists {
		return tss.TssKey{}, fmt.Errorf("key not found: %s", id)
	}
	return key, nil
}

func (m *MockTssKeysDb) SetKey(key tss.TssKey) error {
	m.Keys[key.Id] = key
	return nil
}

func (m *MockTssKeysDb) FindNewKeys(blockHeight uint64) ([]tss.TssKey, error) {
	var results []tss.TssKey
	for _, key := range m.Keys {
		if uint64(key.CreatedHeight) == blockHeight {
			results = append(results, key)
		}
	}
	return results, nil
}

func (m *MockTssKeysDb) FindEpochKeys(epoch uint64) ([]tss.TssKey, error) {
	var results []tss.TssKey
	for _, key := range m.Keys {
		if key.Epoch == epoch {
			results = append(results, key)
		}
	}
	return results, nil
}

// MockTssCommitmentsDb implements tss.TssCommitments interface for testing
type MockTssCommitmentsDb struct {
	aggregate.Plugin
	Commitments map[string]tss.TssCommitment
}

func (m *MockTssCommitmentsDb) SetCommitmentData(commitment tss.TssCommitment) error {
	key := fmt.Sprintf("%s:%d:%s", commitment.KeyId, commitment.Epoch, commitment.Type)
	m.Commitments[key] = commitment
	return nil
}

func (m *MockTssCommitmentsDb) GetCommitment(keyId string, epoch uint64) (tss.TssCommitment, error) {
	for _, commitment := range m.Commitments {
		if commitment.KeyId == keyId && commitment.Epoch == epoch {
			return commitment, nil
		}
	}
	return tss.TssCommitment{}, fmt.Errorf("commitment not found for keyId: %s, epoch: %d", keyId, epoch)
}

func (m *MockTssCommitmentsDb) GetLatestCommitment(keyId string, qtype string) (tss.TssCommitment, error) {
	var latest tss.TssCommitment
	found := false
	for _, commitment := range m.Commitments {
		if commitment.KeyId == keyId && commitment.Type == qtype {
			if !found || commitment.Epoch > latest.Epoch {
				latest = commitment
				found = true
			}
		}
	}
	if !found {
		return tss.TssCommitment{}, fmt.Errorf("no commitment found for keyId: %s, type: %s", keyId, qtype)
	}
	return latest, nil
}

func (m *MockTssCommitmentsDb) GetCommitmentByHeight(
	keyId string,
	height uint64,
	qtype ...string,
) (tss.TssCommitment, error) {
	queryType := ""
	if len(qtype) > 0 {
		queryType = qtype[0]
	}

	var result tss.TssCommitment
	found := false
	for _, commitment := range m.Commitments {
		if commitment.KeyId == keyId && commitment.BlockHeight == height {
			if queryType == "" || commitment.Type == queryType {
				if !found || commitment.Epoch > result.Epoch {
					result = commitment
					found = true
				}
			}
		}
	}
	if !found {
		return tss.TssCommitment{}, fmt.Errorf("commitment not found for keyId: %s, height: %d", keyId, height)
	}
	return result, nil
}

func (m *MockTssCommitmentsDb) GetBlames(opts ...tss.SearchOption) ([]tss.TssCommitment, error) {
	// For testing purposes, we'll return all blame-type commitments
	// In a real MongoDB implementation, SearchOptions would modify a bson.M query
	var results []tss.TssCommitment
	for _, commitment := range m.Commitments {
		if commitment.Type == "blame" {
			results = append(results, commitment)
		}
	}
	return results, nil
}

// MockTssRequestsDb implements tss.TssRequests interface for testing
type MockTssRequestsDb struct {
	aggregate.Plugin
	Requests map[string]tss.TssRequest
}

func (m *MockTssRequestsDb) SetSignedRequest(req tss.TssRequest) error {
	m.Requests[req.Id] = req
	return nil
}

func (m *MockTssRequestsDb) FindUnsignedRequests(blockHeight uint64) ([]tss.TssRequest, error) {
	var results []tss.TssRequest
	for _, req := range m.Requests {
		// Check if request is unsigned (empty signature) and has been created
		if req.Sig == "" && req.Status != tss.SignComplete {
			results = append(results, req)
		}
	}
	return results, nil
}

func (m *MockTssRequestsDb) FindRequests(keyID string, msgs []string) ([]tss.TssRequest, error) {
	var results []tss.TssRequest
	if len(msgs) == 0 {
		return results, nil
	}
	for _, req := range m.Requests {
		if req.KeyId != keyID {
			continue
		}
		if !slices.Contains(msgs, req.Msg) {
			continue
		}
		results = append(results, req)
	}
	return results, nil
}

func (m *MockTssRequestsDb) UpdateRequest(req tss.TssRequest) error {
	if _, exists := m.Requests[req.Id]; !exists {
		return fmt.Errorf("request not found: %s", req.Id)
	}
	m.Requests[req.Id] = req
	return nil
}
