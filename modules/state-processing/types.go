package stateEngine

import "vsc-node/modules/db/vsc/contracts"

type TxPacket struct {
	TxId string
	Ops  []VSCTransaction
}

type TxOutput struct {
	Ok        bool
	RcUsed    int64
	LedgerIds []string
}

type TxResult struct {
	Success bool
	Ret     string
	RcUsed  int64
}

type ContractResult struct {
	Success bool
	Ret     string
	Err     *contracts.ContractOutputError
	TxId    string
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
