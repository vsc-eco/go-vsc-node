package transactions

import a "vsc-node/modules/aggregate"

type Transactions interface {
	a.Plugin
	Ingest(offTx IngestTransactionUpdate) error
	SetOutput(sOut SetResultUpdate)
	GetTransaction(id string) *TransactionRecord
	FindUnconfirmedTransactions(height uint64) ([]TransactionRecord, error)
	SetStatus(ids []string, status string)
}
