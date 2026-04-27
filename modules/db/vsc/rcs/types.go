package rcDb

import (
	"context"
	"vsc-node/modules/aggregate"
)

type RcDb interface {
	aggregate.Plugin
	GetRecord(account string, blockHeight uint64) (RcRecord, error)
	GetRecordsBulk(ctx context.Context, accounts []string, blockHeight uint64) (map[string]RcRecord, error)
	SetRecord(account string, blockHeight uint64, amount int64)
	SetRecordsBulk(ctx context.Context, records []RcRecord) error
	// PruneOlderThan deletes rcs records with block_height < cutoff, but only when a
	// newer record exists for the same account (so GetRecord still has something to
	// return for that account). Safe local-only pruning; no consensus impact.
	PruneOlderThan(ctx context.Context, cutoff uint64) (int64, error)
}

type RcRecord struct {
	Account     string `json:"account" bson:"account"`
	Amount      int64  `json:"amount" bson:"amount"`
	BlockHeight uint64 `json:"block_height" bson:"block_height"`
	MaxRcs      int64  `json:"max_rcs" bson:"-"`
}
