package contracts

import a "vsc-node/modules/aggregate"

type Contracts interface {
	a.Plugin
	RegisterContract(contractId string, args Contract)
	ContractById(contractId string) (Contract, error)
	FindContracts(contractId *string, code *string, offset int, limit int) ([]Contract, error)
}

type ContractState interface {
	a.Plugin
	IngestOutput(inputArgs IngestOutputArgs)
	GetLastOutput(contractId string, height uint64) (ContractOutput, error)
	FindOutputs(id *string, input *string, contract *string, offset int, limit int) ([]ContractOutput, error)
}

type IngestOutputArgs struct {
	Id          string
	ContractId  string
	StateMerkle string

	Inputs  []string
	Results []ContractOutputResult `bson:"results"`

	Metadata map[string]interface{} `bson:"metadata"`

	AnchoredBlock  string
	AnchoredHeight int64
	AnchoredId     string
	AnchoredIndex  int64
}

type ContractOutputResult struct {
	Ret  string               `json:"ret" bson:"ret"`
	Ok   bool                 `json:"ok" bson:"ok"`
	Err  *ContractOutputError `json:"err,omitempty" bson:"err,omitempty"`
	Logs []string             `json:"logs,omitempty" bson:"logs,omitempty"`
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

type ContractOutputError = string

type ContractMetadata struct {
	CurrentSize int `json:"currentSize,omitempty"`
	MaxSize     int `json:"maxSize,omitempty"`
}

const (
	RUNTIME_ERROR       ContractOutputError = "runtime_error"
	RUNTIME_ABORT       ContractOutputError = "runtime_abort"
	RUNTIME_INVALID     ContractOutputError = "runtime_invalid"
	GAS_LIMIT_HIT       ContractOutputError = "gas_limit_hit"
	SDK_ERROR           ContractOutputError = "sdk_error"
	LEDGER_ERROR        ContractOutputError = "ledger_error"
	LEDGER_INTENT_ERROR ContractOutputError = "ledger_intent_error"
)
