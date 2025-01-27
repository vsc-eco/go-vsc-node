package contracts

import a "vsc-node/modules/aggregate"

type Contracts interface {
	a.Plugin
	RegisterContract(contractId string, args SetContractArgs)
}

type SetContractArgs struct {
	Code           string
	Name           string
	Description    string
	Creator        string
	Owner          string
	TxId           string
	CreationHeight int
}

type ContractState interface {
	a.Plugin
	IngestOutput(inputArgs IngestOutputArgs)
	GetLastOutput(contractId string, height int) *ContractOutput
}

type IngestOutputArgs struct {
	Id          string
	Inputs      []string
	ContractId  string
	StateMerkle string
	Gas         struct {
		IO int64
	}
	Results []struct {
		Ret       *string  `bson:"ret"`
		Logs      []string `bson:"logs"`
		Error     *string  `bson:"error"`
		ErrorType *int     `bson:"errorType"`
	} `bson:"results"`
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
