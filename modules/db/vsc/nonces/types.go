package nonces

import a "vsc-node/modules/aggregate"

type Nonces interface {
	a.Plugin
	GetNonce(account string) (NonceRecord, error)
	SetNonce(account string, nonce uint64) error
}

type NonceRecord struct {
	Account string `bson:"account"`
	Nonce   uint64 `bson:"nonce"`
}
