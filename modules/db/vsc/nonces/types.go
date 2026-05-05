package nonces

import (
	"context"
	a "vsc-node/modules/aggregate"
)

type Nonces interface {
	a.Plugin
	GetNonce(ctx context.Context, account string) (NonceRecord, error)
	SetNonce(ctx context.Context, account string, nonce uint64) error
}

type NonceRecord struct {
	Account string `bson:"account"`
	Nonce   uint64 `bson:"nonce"`
}
