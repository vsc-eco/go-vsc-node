package state_engine

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc/contracts"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// Consensus 0.16.0 (consensusversion.SlotObjectSizeBoundActive) keeps the
// objects a block hands to other nodes within the size a node can fetch from a
// peer in one message: each contract's output for the block and the block's
// ledger record. A node that did not build the block fetches both from its
// peers, so keeping each within the message limit keeps every node able to
// fetch it.
//
// The bytes counted are an upper estimate of the encoded size, kept next to
// what they measure: StateEngine.contractOutputBytes with ContractResults (both
// emptied by Flush when a block is applied), LedgerState.OplogBytes with the
// ledger record. Every node that executed the block's transactions counts the
// same. A node that restarted part way through has fewer results in memory, so
// it may accept a call its peers refused; its outputs differ from the block's
// either way, it does not sign that block, and it applies the block as produced
// (outputs and oplog are taken from the block), as it does for any result it
// lost on restart.

// contractResultBytes is an upper estimate of what one result adds to its
// contract's output object: its strings as encoded (a return value that is not
// valid UTF-8 as base64), its logs, its TSS operations as JSON,
// and params.MAX_OUTPUT_RESULT_OVERHEAD for the field names, the encoding and
// its id in the output's inputs list.
func contractResultBytes(r ContractResult) int {
	n := 2*len(r.TxId) + common.EncodedStringBytes(r.ErrMsg) + params.MAX_OUTPUT_RESULT_OVERHEAD
	// MakeOutputs stores a return value that is not valid UTF-8 as base64.
	if utf8.ValidString(r.Ret) {
		n += len(r.Ret)
	} else {
		n += base64.RawStdEncoding.EncodedLen(len(r.Ret))
	}
	if r.Err != nil {
		n += common.EncodedStringBytes(string(*r.Err))
	}
	for _, l := range r.Logs {
		n += common.EncodedStringBytes(l) + 9
	}
	if len(r.TssOps) > 0 {
		if b, err := json.Marshal(r.TssOps); err == nil {
			n += len(b)
		} else {
			n += params.MAX_OUTPUT_RESULT_OVERHEAD
		}
	}
	return n
}

// addedOutputBytes sums contractResultBytes of outs per contract.
func addedOutputBytes(outs []ContractIdResult) map[string]int {
	added := make(map[string]int, len(outs))
	for _, o := range outs {
		added[o.ContractId] += contractResultBytes(o.Output)
	}
	return added
}

// slotObjectsOverBound reports why a transaction that succeeded must fail
// instead under the size bound, or "" when it fits: its outputs would take a
// contract's output for the block over params.MAX_CONTRACT_OUTPUT_BYTES, or its
// ledger operations (pendingOplogBytes) would take the block's ledger record
// over params.MAX_BLOCK_OPLOG_BYTES. Contracts are checked in sorted order, so
// the reason names the same contract on every node.
func (se *StateEngine) slotObjectsOverBound(outs []ContractIdResult, pendingOplogBytes int) string {
	added := addedOutputBytes(outs)
	ids := make([]string, 0, len(added))
	for id := range added {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if have := se.contractOutputBytes[id]; have+added[id] > params.MAX_CONTRACT_OUTPUT_BYTES {
			return fmt.Sprintf("the output of contract %s for this block would be %d bytes, over the %d-byte limit (%d already used)",
				id, have+added[id], params.MAX_CONTRACT_OUTPUT_BYTES, have)
		}
	}
	if pendingOplogBytes > 0 && se.LedgerState != nil {
		if have := se.LedgerState.OplogBytes; have+pendingOplogBytes > params.MAX_BLOCK_OPLOG_BYTES {
			return fmt.Sprintf("the ledger record of this block would be %d bytes, over the %d-byte limit (%d already used)",
				have+pendingOplogBytes, params.MAX_BLOCK_OPLOG_BYTES, have)
		}
	}
	return ""
}

// boundFailedResult keeps a failed result inside its contract's bound, given
// what the contract's output for the block already holds: the revert symbol
// (contract-chosen text) is cut to params.MAX_ERROR_SYMBOL_BYTES, and the
// error message to what still fits. fits is false when even the result
// without a message does not fit: the caller leaves it out. Cuts fall on a
// UTF-8 boundary, so the strings stay valid to encode.
func boundFailedResult(r ContractResult, used int) (ContractResult, bool) {
	if r.Err != nil && len(*r.Err) > params.MAX_ERROR_SYMBOL_BYTES {
		cut := contracts.ContractOutputError(cutUTF8(string(*r.Err), params.MAX_ERROR_SYMBOL_BYTES))
		r.Err = &cut
	}
	size := contractResultBytes(r)
	if used+size > params.MAX_CONTRACT_OUTPUT_BYTES {
		room := params.MAX_CONTRACT_OUTPUT_BYTES - used - (size - common.EncodedStringBytes(r.ErrMsg))
		r.ErrMsg = cutUTF8(r.ErrMsg, room)
		size = contractResultBytes(r)
	}
	return r, used+size <= params.MAX_CONTRACT_OUTPUT_BYTES
}

// failedResultsOnly drops the successful results of a transaction that failed
// (consensus 0.16.0): the transaction was reverted, so none of its calls'
// results are published.
func failedResultsOnly(outs []ContractIdResult) []ContractIdResult {
	kept := outs[:0]
	for _, o := range outs {
		if !o.Output.Success {
			kept = append(kept, o)
		}
	}
	return kept
}

// CallOutputs is the results one contract call leaves in the block's contract
// outputs: the called contract's (its return value and logs) and one for each
// other contract it reached that logged or asked for TSS operations, in
// contract id order. ExecuteBatch, the dry run and the contract test harness
// all build them here, so the size bound counts the same bytes in each.
func CallOutputs(txId, contractId, ret string, success bool, logs map[string]contract_session.LogOutput) []ContractIdResult {
	// Sort log keys for deterministic output ordering across nodes.
	// Cross-contract calls produce logs from multiple contracts;
	// unsorted map iteration causes different output order → CID mismatch.
	ids := make([]string, 0, len(logs))
	for id := range logs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	outs := make([]ContractIdResult, 0, len(ids))
	for _, id := range ids {
		l := logs[id]
		r := ContractResult{TxId: txId, Success: success, Logs: l.Logs, TssOps: l.TssOps}
		if id == contractId {
			r.Ret = ret
		}
		outs = append(outs, ContractIdResult{ContractId: id, Output: r})
	}
	return outs
}

// cutUTF8 returns s cut to at most n bytes ("" for n <= 0), without the
// incomplete character the cut may leave at the end.
func cutUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for i := 0; i < utf8.UTFMax && len(s) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// outputTooLarge is the failed result recorded for a call that would have
// overflowed the bound.
func outputTooLarge(txId, reason string) ContractResult {
	code := contracts.OUTPUT_TOO_LARGE
	return ContractResult{TxId: txId, Success: false, Err: &code, ErrMsg: reason}
}

// WithCalledContract is logs with an entry for the called contract, as the
// chain has: TxVscCallContract.ExecuteTx opens the called contract's session
// before it runs, so its result is in the block's output even when it logged
// nothing and touched no state. A dry run or the test harness does not open
// it, so they add it here before CallOutputs.
func WithCalledContract(logs map[string]contract_session.LogOutput, contractId string) map[string]contract_session.LogOutput {
	if _, ok := logs[contractId]; ok {
		return logs
	}
	out := make(map[string]contract_session.LogOutput, len(logs)+1)
	for id, l := range logs {
		out[id] = l
	}
	out[contractId] = contract_session.LogOutput{}
	return out
}

// DryRunOverBound is the size bound for a dry run of a sequence of calls
// (gql simulateContractCalls) and the contract test harness. used holds what
// the calls simulated so far added per contract; outs is this call's
// CallOutputs. It cannot see the block's other transactions, so it bounds only
// what the simulated calls add themselves; the chain may still refuse a call
// that fits here because the block is full. It returns the reason the chain
// would fail the call, or "" and adds the call's bytes to used.
func DryRunOverBound(used map[string]int, outs []ContractIdResult, pendingOplogBytes int) string {
	probe := &StateEngine{contractOutputBytes: used, LedgerState: &ledgerSystem.LedgerState{}}
	if reason := probe.slotObjectsOverBound(outs, pendingOplogBytes); reason != "" {
		return reason
	}
	for id, n := range addedOutputBytes(outs) {
		used[id] += n
	}
	return ""
}
