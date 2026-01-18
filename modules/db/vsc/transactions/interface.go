package transactions

import a "vsc-node/modules/aggregate"

type Transactions interface {
	a.Plugin
	Ingest(offTx IngestTransactionUpdate) error
	SetOutput(sOut SetResultUpdate)
	GetTransaction(id string) *TransactionRecord
	FindTransactions(ids []string, id *string, account *string, contract *string, status *TransactionStatus, byType []string, ledgerToFrom *string, ledgerTypes []string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]TransactionRecord, error)
	FindUnconfirmedTransactions(height uint64) ([]TransactionRecord, error)
	// SetStatus(ids []string, status string)
}
