package state_engine

import (
	"vsc-node/modules/db/vsc/contracts"
	tss_db "vsc-node/modules/db/vsc/tss"
)

type TxPacket struct {
	TxId string
	Ops  []VSCTransaction

	// Invalid marks a transaction that could not be resolved into executable ops
	// (unknown op type, or an undecodable attacker-controlled payload). Such a tx
	// executes NO op and is marked FAILED by ExecuteBatch's early-fail branch.
	// Payer (RequiredAuths[0]) is charged a fixed RC so an invalid tx isn't free.
	// Set only on the offchain ingest path; the L1 path leaves both zero-valued.
	Invalid bool
	Payer   string
}

type TxOutput struct {
	Ok        bool
	RcUsed    int64
	LedgerIds []string
}

type TxResult struct {
	Success bool
	Err     *contracts.ContractOutputError
	Ret     string
	RcUsed  int64
}

type ContractIdResult struct {
	ContractId string
	Output     ContractResult
}

type ContractResult struct {
	Success bool
	Ret     string
	Err     *contracts.ContractOutputError
	ErrMsg  string
	TxId    string
	Logs    []string
	TssOps  []tss_db.TssOp
}

// More information about the TX
type TxSelf struct {
	TxId                 string
	BlockId              string
	BlockHeight          uint64
	Index                int
	OpIndex              int
	Timestamp            string
	RequiredAuths        []string
	RequiredPostingAuths []string
}

type OplogOutputEntry struct {
	Id        string `json:"id" bson:"id"`
	Ok        bool   `json:"ok" bson:"ok"`
	LedgerIdx []int  `json:"lidx" bson:"lidx"`
}
