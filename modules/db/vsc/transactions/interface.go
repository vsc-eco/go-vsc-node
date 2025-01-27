package transactions

import a "vsc-node/modules/aggregate"

type Transactions interface {
	a.Plugin
	Ingest(offTx IngestTransactionUpdate) error
	SetOutput(sOut SetOutputUpdate)
	GetTransaction(id string) *TransactionRecord
}
