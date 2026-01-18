package contracts

import (
	a "vsc-node/modules/aggregate"
	tss_db "vsc-node/modules/db/vsc/tss"
)

type Contracts interface {
	a.Plugin
	RegisterContract(contractId string, args Contract)
	ContractById(contractId string, height uint64) (Contract, error)
	FindContracts(contractId *string, code *string, historical *bool, offset int, limit int) ([]Contract, error)
}

type ContractState interface {
	a.Plugin
	IngestOutput(inputArgs IngestOutputArgs)
	GetLastOutput(contractId string, height uint64) (ContractOutput, error)
	FindOutputs(id *string, input *string, contract *string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ContractOutput, error)
}

type IngestOutputArgs struct {
	Id          string
	ContractId  string
	StateMerkle string

	Inputs  []string
	Results []ContractOutputResult `bson:"results"`

	Metadata ContractMetadata `bson:"metadata"`

	AnchoredBlock  string
	AnchoredHeight int64
	AnchoredId     string
	AnchoredIndex  int64
}

type ContractOutputResult struct {
	Ret    string               `json:"ret" bson:"ret"`
	Ok     bool                 `json:"ok" bson:"ok"`
	Err    *ContractOutputError `json:"err,omitempty" bson:"err,omitempty"`
	ErrMsg string               `json:"errMsg,omitempty" bson:"errMsg,omitempty"`
	Logs   []string             `json:"logs,omitempty" bson:"logs,omitempty"`
	TssOps []tss_db.TssOp       `json:"tss_ops,omitempty" bson:"tss_ops,omitempty"`
}

type ContractOutput struct {
	Id          string           `json:"id"`
	BlockHeight int64            `json:"block_height" bson:"block_height"`
	Timestamp   *string          `json:"timestamp,omitempty" bson:"timestamp,omitempty"`
	ContractId  string           `json:"contract_id" bson:"contract_id"`
	Inputs      []string         `json:"inputs"`
	Metadata    ContractMetadata `json:"metadata"`
	//This might not be used

	Results     []ContractOutputResult `json:"results" bson:"results"`
	StateMerkle string                 `json:"state_merkle" bson:"state_merkle"`
}

type ContractUpdate struct {
	Id          string  `json:"id" bson:"_id"`
	ContractId  string  `json:"contract_id" bson:"contract_id"`
	BlockHeight int64   `json:"block_height" bson:"block_height"`
	Ts          *string `json:"ts,omitempty" bson:"ts,omitempty"`
	Owner       string  `json:"owner" bson:"owner"`
	Code        string  `json:"code" bson:"code"`
}

type ContractOutputError = string

type ContractMetadata struct {
	CurrentSize int `json:"currentSize,omitempty"`
	MaxSize     int `json:"maxSize,omitempty"`
}

type ICCallOptions struct {
	Intents []Intent `json:"intents,omitempty"`
}

const (
	RUNTIME_ERROR       ContractOutputError = "runtime_error"
	RUNTIME_ABORT       ContractOutputError = "runtime_abort"
	RUNTIME_INVALID     ContractOutputError = "runtime_invalid"
	IC_INVALD_PAYLD     ContractOutputError = "ic_invalid_payload"
	IC_CONTRT_NOT_FND   ContractOutputError = "ic_contract_not_found"
	IC_CONTRT_GET_ERR   ContractOutputError = "ic_contract_get_error"
	IC_CODE_FET_ERR     ContractOutputError = "ic_code_fetch_error"
	IC_CID_DEC_ERR      ContractOutputError = "ic_cid_decode_error"
	IC_RCSE_LIMIT_HIT   ContractOutputError = "ic_recursion_limit_hit"
	GAS_LIMIT_HIT       ContractOutputError = "gas_limit_hit"
	SDK_ERROR           ContractOutputError = "sdk_error"
	MISSING_REQ_AUTH    ContractOutputError = "missing_required_auth"
	ENV_VAR_ERROR       ContractOutputError = "env_var_error"
	LEDGER_ERROR        ContractOutputError = "ledger_error"
	LEDGER_INTENT_ERROR ContractOutputError = "ledger_intent_error"
	WASM_INIT_ERROR     ContractOutputError = "wasm_init_error"
	WASM_RET_ERROR      ContractOutputError = "wasm_ret_error"
	WASM_FUNC_NOT_FND   ContractOutputError = "wasm_function_not_found"
	UNK_ERROR           ContractOutputError = "unknown_error"
	TSS_KEY_NOT_FOUND   ContractOutputError = "tss_key_not_found"
	TSS_KEY_DUPLICATE   ContractOutputError = "tss_key_duplicate"
)
