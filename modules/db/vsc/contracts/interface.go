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
