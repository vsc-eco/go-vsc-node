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
	GetLastOutput(contractId string, height uint64) *ContractOutput
}

type IngestOutputArgs struct {
	Id          string
	ContractId  string
	StateMerkle string

	Inputs  []string
	Results []struct {
		Ret string `json:"ret" bson:"ret"`
		Ok  bool   `json:"ok" bson:"ok"`
	} `bson:"results"`

	Metadata map[string]interface{} `bson:"metadata"`

	AnchoredBlock  string
	AnchoredHeight int64
	AnchoredId     string
	AnchoredIndex  int64
}

type ContractOutput struct {
	Id          string   `bson:"id"`
	Inputs      []string `bson:"inputs"`
	ContractId  string   `bson:"contract_id"`
	StateMerkle string   `bson:"state_merkle"`

	Results []struct {
		Ret       *string  `bson:"ret"`
		Logs      []string `bson:"logs"`
		Error     *string  `bson:"error"`
		ErrorType *int     `bson:"errorType"`
	} `bson:"results"`
	BlockHeight int64 `bson:"block_height"`
}
