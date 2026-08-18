package rcDb

import "vsc-node/modules/aggregate"

type RcDb interface {
	aggregate.Plugin
	GetRecord(account string, blockHeight uint64) (RcRecord, error)
	// SetRecord persists an account's RC consumption for a slot. It returns an
	// error: the caller clears the in-memory consumption immediately after, so
	// a silently-dropped write loses that slot's RC accounting with nothing to
	// rebuild it from (unlike balances, RCs are not a fold of an append-only
	// log). RC gates transaction admission, so a node that under-counts admits
	// transactions its peers reject.
	SetRecord(account string, blockHeight uint64, amount int64) error
}

type RcRecord struct {
	Account     string `json:"account" bson:"account"`
	Amount      int64  `json:"amount" bson:"amount"`
	BlockHeight uint64 `json:"block_height" bson:"block_height"`
	MaxRcs      int64  `json:"max_rcs" bson:"-"`
}
