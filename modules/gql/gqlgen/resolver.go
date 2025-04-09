package gqlgen

import (
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

type Resolver struct {
	Witnesses   witnesses.Witnesses
	TxPool      *transactionpool.TransactionPool
	Balances    ledgerDb.Balances
	StateEngine *stateEngine.StateEngine
	HiveBlocks  hive_blocks.HiveBlocks
}
