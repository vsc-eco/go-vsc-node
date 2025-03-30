package rcDb

import "vsc-node/modules/aggregate"

type RcDb interface {
	aggregate.Plugin
	GetRecord(account string, blockHeight uint64) (RcRecord, error)
	SetRecord(account string, blockHeight uint64, amount int64)
}

type RcRecord struct {
	Account     string
	Amount      int64
	BlockHeight uint64
}
