package transactions

import (
	"context"
	a "vsc-node/modules/aggregate"
)

type Transactions interface {
	a.Plugin
	Ingest(ctx context.Context, offTx IngestTransactionUpdate) error
	SetOutput(ctx context.Context, sOut SetResultUpdate)
	GetTransaction(ctx context.Context, id string) *TransactionRecord
	FindTransactions(ctx context.Context, ids []string, id *string, account *string, contract *string, status *TransactionStatus, byType []string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]TransactionRecord, error)
	FindUnconfirmedTransactions(ctx context.Context, height uint64) ([]TransactionRecord, error)
	InvalidateCompetingTransactions(ctx context.Context, requiredAuths []string, nonces []uint64) (int64, error)
	HasUnconfirmedWithNonce(ctx context.Context, requiredAuths []string, nonce uint64) (bool, error)
}
