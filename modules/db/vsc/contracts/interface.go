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

type ContractHistory interface {
	a.Plugin
	IngestOutput(inputArgs IngestOutputArgs)
}

type IngestOutputArgs struct {
	Id          string
	Inputs      []string
	ContractId  string
	StateMerkle string
	Gas         struct {
		IO int64
	}
	AnchoredBlock  string
	AnchoredHeight int64
	AnchoredId     string
	AnchoredIndex  int64
}
