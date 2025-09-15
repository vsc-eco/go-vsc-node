package contract_session

import (
	"maps"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/db/vsc/contracts"

	"github.com/ipfs/go-cid"
)

// Session for transaction with contract calls
type CallSession struct {
	dl       *datalayer.DataLayer
	stateDb  contracts.ContractState
	lastBh   uint64
	sessions map[string]*ContractSession
}

// Create a new contract call session for a transaction.
func NewCallSession(dl *datalayer.DataLayer, stateDb contracts.ContractState, lastBh uint64) *CallSession {
	return &CallSession{
		dl:       dl,
		stateDb:  stateDb,
		lastBh:   lastBh,
		sessions: make(map[string]*ContractSession),
	}
}

// Load output from last se.TempOutputs. This will create a new contract session from the specified output.
func (cs *CallSession) FromOutput(contractId string, out TempOutput) {
	sess := NewContractSession(cs.dl, out)
	cs.sessions[contractId] = sess
}

// Get session of a contract. A contract session will be created from the last contract output stored in state DB if not exists already.
func (cs *CallSession) GetContractSession(contractId string) *ContractSession {
	_, exists := cs.sessions[contractId]
	if !exists {
		lastOutput, err := cs.stateDb.GetLastOutput(contractId, cs.lastBh)

		var cid string
		metadata := contracts.ContractMetadata{}

		if err == nil {
			cid = lastOutput.StateMerkle
			metadata = lastOutput.Metadata
		}

		tmpOut := TempOutput{
			Cache:     make(map[string][]byte),
			Metadata:  metadata,
			Deletions: make(map[string]bool),
			Cid:       cid,
		}

		cs.FromOutput(contractId, tmpOut)
	}
	sess := cs.sessions[contractId]
	return sess
}

// Get current state store of a contract.
func (cs *CallSession) GetStateStore(contractId string) *StateStore {
	sess := cs.GetContractSession(contractId)
	return sess.GetStateStore()
}

// Get metadata of a contract.
func (cs *CallSession) GetMetadata(contractId string) contracts.ContractMetadata {
	sess := cs.GetContractSession(contractId)
	return sess.GetMetadata()
}

// Set metadata of a contract.
func (cs *CallSession) SetMetadata(contractId string, meta contracts.ContractMetadata) {
	sess := cs.GetContractSession(contractId)
	sess.SetMetadata(meta)
}

// Retrieve all contract outputs in the contract call.
func (cs *CallSession) ToOutputs() map[string]TempOutput {
	result := make(map[string]TempOutput)
	for id, session := range cs.sessions {
		result[id] = session.ToOutput()
	}
	return result
}

// Append logs for a contract
func (cs *CallSession) AppendLogs(contractId string, logs []string) {
	sess := cs.GetContractSession(contractId)
	sess.AppendLogs(logs)
}

// Pop all logs from contract sessions and return them
func (cs *CallSession) PopLogs() map[string][]string {
	result := make(map[string][]string)
	for id, session := range cs.sessions {
		result[id] = session.PopLogs()
	}
	return result
}

// Commit state changes to contract sessions
func (cs *CallSession) Commit() {
	for _, session := range cs.sessions {
		session.GetStateStore().Commit()
	}
}

// Rollback state changes
func (cs *CallSession) Rollback() {
	for _, session := range cs.sessions {
		session.state.Rollback()
	}
}

// Session for a contract
type ContractSession struct {
	dl *datalayer.DataLayer

	metadata    contracts.ContractMetadata
	cache       map[string][]byte
	deletions   map[string]bool
	stateMerkle string
	state       *StateStore
	logs        []string
}

func NewContractSession(dl *datalayer.DataLayer, output TempOutput) *ContractSession {
	newSession := &ContractSession{
		dl:          dl,
		metadata:    output.Metadata,
		cache:       output.Cache,
		deletions:   output.Deletions,
		stateMerkle: output.Cid,
		logs:        make([]string, 0),
	}
	newSession.state = NewStateStore(dl, output.Cid, newSession)
	return newSession
}

// Get the current state store of the contract
func (cs *ContractSession) GetStateStore() *StateStore {
	if cs.state == nil {
		cs.state = NewStateStore(cs.dl, cs.stateMerkle, cs)
	}
	return cs.state
}

func (cs *ContractSession) StateGet(key string) []byte {
	if cs.deletions[key] {
		return nil
	}
	return cs.cache[key]
}

func (cs *ContractSession) GetMetadata() contracts.ContractMetadata {
	return cs.metadata
}

func (cs *ContractSession) SetMetadata(meta contracts.ContractMetadata) {
	cs.metadata = meta
}

func (cs *ContractSession) ToOutput() TempOutput {
	return TempOutput{
		Cache:     cs.cache,
		Cid:       cs.stateMerkle,
		Metadata:  cs.metadata,
		Deletions: cs.deletions,
	}
}

func (cs *ContractSession) AppendLogs(logs []string) {
	cs.logs = append(cs.logs, logs...)
}

func (cs *ContractSession) PopLogs() []string {
	// TODO: walk through inter-contract call sessions and return their logs
	popped := cs.logs
	cs.logs = make([]string, 0)
	return popped
}

type StateStore struct {
	cache     map[string][]byte
	deletions map[string]bool
	datalayer *datalayer.DataLayer
	databin   *datalayer.DataBin
	cs        *ContractSession
}

func (ss *StateStore) Get(key string) []byte {
	if ss.deletions[key] {
		return []byte{}
	}
	// return ss.cache[key]
	if ss.cache[key] == nil {
		if val := ss.cs.StateGet(key); val != nil {
			return val
		}
		cidz, err := ss.databin.Get(key)

		if err == nil {
			rawBytes, err := ss.datalayer.GetRaw(*cidz)
			if err == nil {
				ss.cache[key] = rawBytes
			} else {
				ss.cache[key] = make([]byte, 0)
			}
		} else {
			ss.cache[key] = nil
		}
	}

	return ss.cache[key]
}

func (ss *StateStore) Set(key string, value []byte) {
	ss.cache[key] = value
	delete(ss.deletions, key)
}

func (ss *StateStore) Delete(key string) {
	delete(ss.cache, key)
	ss.deletions[key] = true
}

func (ss *StateStore) Commit() {
	// commit the changes to the underlying storage
	ss.cs.deletions = ss.deletions
	ss.cs.cache = ss.cache
}

func (ss *StateStore) Rollback() {
	ss.deletions = make(map[string]bool)
	ss.cache = make(map[string][]byte)
}

func NewStateStore(dl *datalayer.DataLayer, cids string, cs *ContractSession) *StateStore {
	if cids == "" {
		databin := datalayer.NewDataBin(dl)

		return &StateStore{
			cache:     maps.Clone(cs.cache),
			deletions: maps.Clone(cs.deletions),
			datalayer: dl,
			databin:   &databin,
			cs:        cs,
		}
	} else {
		cidz := cid.MustParse(cids)
		databin := datalayer.NewDataBinFromCid(dl, cidz)

		return &StateStore{
			cache:     maps.Clone(cs.cache),
			deletions: maps.Clone(cs.deletions),
			datalayer: dl,
			databin:   &databin,
			cs:        cs,
		}
	}
}

type TempOutput struct {
	Cache map[string][]byte

	Metadata  contracts.ContractMetadata
	Deletions map[string]bool
	Cid       string
}
