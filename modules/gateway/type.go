package gateway

import "github.com/vsc-eco/hivego"

type ChainAction struct {
	Ops        []string `json:"ops"`
	ClearedOps string   `json:"cleared_ops"`
}

type signingPackage struct {
	Ops  []hivego.HiveOperation
	Tx   hivego.HiveTransaction
	TxId string
}
