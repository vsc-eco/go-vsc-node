package gqlgen

import (
	"vsc-node/lib/datalayer"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/witnesses"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

type Resolver struct {
	Witnesses      witnesses.Witnesses
	TxPool         *transactionpool.TransactionPool
	Balances       ledgerDb.Balances
	Ledger         ledgerDb.Ledger
	Actions        ledgerDb.BridgeActions
	Elections      elections.Elections
	Transactions   transactions.Transactions
	Nonces         nonces.Nonces
	Rc             rcDb.RcDb
	HiveBlocks     hive_blocks.HiveBlocks
	StateEngine    *stateEngine.StateEngine
	Da             *datalayer.DataLayer
	Contracts      contracts.Contracts
	ContractsState contracts.ContractState
	TssKeys        tss_db.TssKeys
	TssRequests    tss_db.TssRequests
}
