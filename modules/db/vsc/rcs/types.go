package rcDb

import (
	"context"
	"vsc-node/modules/aggregate"
)

type RcDb interface {
	aggregate.Plugin
	GetRecord(ctx context.Context, account string, blockHeight uint64) (RcRecord, error)
	SetRecord(ctx context.Context, account string, blockHeight uint64, amount int64)
}

type RcRecord struct {
	Account     string `json:"account" bson:"account"`
	Amount      int64  `json:"amount" bson:"amount"`
	BlockHeight uint64 `json:"block_height" bson:"block_height"`
	MaxRcs      int64  `json:"max_rcs" bson:"-"`
}
