package gqlgen

import (
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	transactionpool "vsc-node/modules/transaction-pool"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

type Resolver struct {
	Witnesses witnesses.Witnesses
	TxPool    *transactionpool.TransactionPool
	Balances  ledgerDb.Balances
}
