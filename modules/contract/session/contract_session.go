package contract_session

import (
	"maps"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/db/vsc/contracts"

	"github.com/ipfs/go-cid"
)

type ContractSession struct {
	dl *datalayer.DataLayer

	metadata    contracts.ContractMetadata
	cache       map[string][]byte
	deletions   map[string]bool
	stateMerkle string
	logs        []string

	// stateSesions map[string]*StateStore
}

func New(dl *datalayer.DataLayer) *ContractSession {
	return &ContractSession{
		dl:   dl,
		logs: make([]string, 0),
	}
}

// Longer term this should allow for getting from multiple contracts
// This just does the only contract here
func (cs *ContractSession) GetStateStore(contractId ...string) *StateStore {
	ss := NewStateStore(cs.dl, cs.stateMerkle, cs)
	return &ss
	// if cs.stateSesions[contractId] != nil {
	// 	txOutput := cs.stateEngine.VirtualOutputs[contractId]

	// 	contractOutput := cs.stateEngine.contractState.GetLastOutput(contractId, cs.bh)

	// 	cidz := cid.MustParse(contractOutput.StateMerkle)

	// 	ss := NewStateStore(cs.stateEngine.da, cidz)

	// 	cs.stateSesions[contractId] = &ss

	// 	return &ss
	// } else {
	// 	return cs.stateSesions[contractId]
	// }
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

func (cs *ContractSession) FromOutput(output TempOutput) {
	cs.cache = output.Cache
	cs.metadata = output.Metadata
	cs.stateMerkle = output.Cid
	cs.deletions = make(map[string]bool)
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

func NewStateStore(dl *datalayer.DataLayer, cids string, cs *ContractSession) StateStore {
	if cids == "" {
		databin := datalayer.NewDataBin(dl)

		return StateStore{
			cache:     maps.Clone(cs.cache),
			deletions: maps.Clone(cs.deletions),
			datalayer: dl,
			databin:   &databin,
			cs:        cs,
		}
	} else {
		cidz := cid.MustParse(cids)
		databin := datalayer.NewDataBinFromCid(dl, cidz)

		return StateStore{
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
