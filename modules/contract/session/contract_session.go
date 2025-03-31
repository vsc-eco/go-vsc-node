package contract_session

import (
	"vsc-node/lib/datalayer"

	"github.com/ipfs/go-cid"
)

type ContractSession struct {
	dl *datalayer.DataLayer

	metadata    map[string]interface{}
	cache       map[string][]byte
	deletions   map[string]bool
	stateMerkle string

	// stateSesions map[string]*StateStore
}

func New(dl *datalayer.DataLayer) *ContractSession {
	return &ContractSession{
		dl: dl,
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

func (cs *ContractSession) GetMetadata() map[string]interface{} {
	return cs.metadata
}

func (cs *ContractSession) SetMetadata(meta map[string]interface{}) {
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
}

type StateStore struct {
	cache     map[string][]byte
	deletions map[string]bool
	datalayer *datalayer.DataLayer
	databin   *datalayer.DataBin
	cs        *ContractSession
}

func (ss *StateStore) Get(key string) []byte {
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
			cache:     make(map[string][]byte),
			datalayer: dl,
			databin:   &databin,
			cs:        cs,
		}
	} else {
		cidz := cid.MustParse(cids)
		databin := datalayer.NewDataBinFromCid(dl, cidz)

		return StateStore{
			cache:     make(map[string][]byte),
			datalayer: dl,
			databin:   &databin,
			cs:        cs,
		}
	}
}

type TempOutput struct {
	Cache map[string][]byte

	Metadata  map[string]interface{}
	Deletions map[string]bool
	Cid       string
}
