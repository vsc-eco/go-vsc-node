package contract_session

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/db/vsc/contracts"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/JustinKnueppel/go-result"
	"github.com/ipfs/go-cid"
	"go.mongodb.org/mongo-driver/mongo"
)

type ContractWithCode struct {
	Info contracts.Contract
	Code []byte
}

type LogOutput struct {
	Logs   []string
	TssOps []tss_db.TssOp
}

// Session for transaction with contract calls
type CallSession struct {
	dl         *datalayer.DataLayer
	contractDb contracts.Contracts
	stateDb    contracts.ContractState
	lastBh     uint64
	sessions   map[string]*ContractSession

	TssKeys tss_db.TssKeys

	// pending carries the temp contract outputs already produced earlier in the slot.
	pending map[string]*TempOutput
}

// Create a new contract call session for a transaction.
func NewCallSession(dl *datalayer.DataLayer, contractDb contracts.Contracts, stateDb contracts.ContractState, tssKeys tss_db.TssKeys, lastBh uint64, pending map[string]*TempOutput) *CallSession {
	return &CallSession{
		dl:         dl,
		contractDb: contractDb,
		stateDb:    stateDb,
		lastBh:     lastBh,
		sessions:   make(map[string]*ContractSession),
		TssKeys:    tssKeys,
		pending:    cloneTempOutputs(pending), // Copy temp outputs so mutations here never leak back to the caller.
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
		// Prefer the latest in-memory output before falling back to Mongo.
		if tmpOut := cs.takePending(contractId); tmpOut != nil {
			cs.FromOutput(contractId, *tmpOut)
		} else {
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

// Increment current size of a contract in metadata. Returns an int of any increases of max size.
func (cs *CallSession) IncSize(contractId string, size int) int {
	sess := cs.GetContractSession(contractId)
	return sess.IncSize(size)
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
func (cs *CallSession) AppendLogs(contractId string, logs ...string) {
	sess := cs.GetContractSession(contractId)
	sess.AppendLogs(logs)
}

// Pop all logs from contract sessions and return them
func (cs *CallSession) PopLogs() map[string]LogOutput {
	result := make(map[string]LogOutput)
	for id, session := range cs.sessions {
		result[id] = LogOutput{
			Logs:   session.PopLogs(),
			TssOps: session.PopTssLogs(),
		}
	}
	return result
}

func (cs *CallSession) AppendTssLog(contractId string, op tss_db.TssOp) {
	session := cs.GetContractSession(contractId)
	session.tssOps = append(session.tssOps, op)
}

// pulls (and removes) the latest in-memory temp output for a contract
// so the session can read the freshest state of the slot before hitting the DB.
func (cs *CallSession) takePending(contractId string) *TempOutput {
	if cs.pending == nil {
		return nil
	}
	tmp, exists := cs.pending[contractId]
	if !exists {
		return nil
	}
	// Drop the entry so later DB loads pick up the newest committed state instead.
	delete(cs.pending, contractId)
	return cloneTempOutput(tmp)
}

// func (cs *CallSession) TssLogs() []TssOp {
// 	return cs.tssOps
// }

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
		session.PopLogs()
	}
}

func (cs *CallSession) GetStateDiff() map[string]StateDiff {
	res := make(map[string]StateDiff)
	for id, session := range cs.sessions {
		res[id] = session.state.GetDiff()
	}
	return res
}

func (cs *CallSession) GetContractFromDb(contractId string, height uint64) result.Result[ContractWithCode] {
	info, err := cs.contractDb.ContractById(contractId, height)
	if err == mongo.ErrNoDocuments {
		return result.Err[ContractWithCode](errors.Join(fmt.Errorf(contracts.IC_CONTRT_NOT_FND), fmt.Errorf("contract not found")))
	} else if err != nil {
		return result.Err[ContractWithCode](errors.Join(fmt.Errorf(contracts.IC_CONTRT_GET_ERR), err))
	}
	c, err := cid.Decode(info.Code)
	if err != nil {
		return result.Err[ContractWithCode](errors.Join(fmt.Errorf(contracts.IC_CID_DEC_ERR), err))
	}
	node, err := cs.dl.Get(c, nil)
	if err != nil {
		return result.Err[ContractWithCode](errors.Join(fmt.Errorf(contracts.IC_CODE_FET_ERR), err))
	}
	code := node.RawData()
	return result.Ok(ContractWithCode{info, code})
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

	tssOps []tss_db.TssOp
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

func (cs *ContractSession) IncSize(size int) int {
	cs.metadata.CurrentSize += size
	newWriteGas := max(0, cs.metadata.CurrentSize-cs.metadata.MaxSize)
	cs.metadata.MaxSize = max(cs.metadata.MaxSize, cs.metadata.CurrentSize)
	return newWriteGas
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
	popped := cs.logs
	cs.logs = make([]string, 0)
	return popped
}

func (cs *ContractSession) PopTssLogs() []tss_db.TssOp {
	popped := cs.tssOps
	cs.tssOps = make([]tss_db.TssOp, 0)
	return popped
}

type StateStore struct {
	cache     map[string][]byte
	deletions map[string]bool
	datalayer *datalayer.DataLayer
	databin   *datalayer.DataBin
	cs        *ContractSession
}

type stateKeyDiff struct {
	Previous []byte
	Current  []byte
}

type StateDiff struct {
	Deletions map[string]bool
	KeyDiff   map[string]stateKeyDiff
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
	ss.cs.deletions = maps.Clone(ss.deletions)
	ss.cs.cache = maps.Clone(ss.cache)
}

func (ss *StateStore) Rollback() {
	// revert to last committed state
	ss.deletions = maps.Clone(ss.cs.deletions)
	ss.cache = maps.Clone(ss.cs.cache)
}

// Return a summary of uncommitted state diff
func (ss *StateStore) GetDiff() StateDiff {
	dels := make(map[string]bool)
	diffs := make(map[string]stateKeyDiff)

	for key := range ss.deletions {
		if !ss.cs.deletions[key] {
			dels[key] = true
		}
	}
	for key, val := range ss.cache {
		if !bytes.Equal(val, ss.cs.cache[key]) {
			diffs[key] = stateKeyDiff{
				Previous: ss.cs.cache[key],
				Current:  val,
			}
		}
	}

	return StateDiff{
		Deletions: dels,
		KeyDiff:   diffs,
	}
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

func cloneTempOutputs(src map[string]*TempOutput) map[string]*TempOutput {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]*TempOutput, len(src))
	for k, v := range src {
		dst[k] = cloneTempOutput(v)
	}
	return dst
}

func cloneTempOutput(src *TempOutput) *TempOutput {
	if src == nil {
		return nil
	}
	cloned := TempOutput{
		Metadata: src.Metadata,
		Cid:      src.Cid,
	}
	if len(src.Cache) > 0 {
		cloned.Cache = make(map[string][]byte, len(src.Cache))
		for k, v := range src.Cache {
			if v == nil {
				cloned.Cache[k] = nil
				continue
			}
			cloned.Cache[k] = slices.Clone(v)
		}
	} else {
		cloned.Cache = make(map[string][]byte)
	}
	if len(src.Deletions) > 0 {
		cloned.Deletions = maps.Clone(src.Deletions)
	} else {
		cloned.Deletions = make(map[string]bool)
	}
	return &cloned
}
