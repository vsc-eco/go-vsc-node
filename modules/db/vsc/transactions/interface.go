package transactions

import (
	"context"
	a "vsc-node/modules/aggregate"
)

type Transactions interface {
	a.Plugin
	Ingest(offTx IngestTransactionUpdate) error
	SetOutput(sOut SetResultUpdate)
	GetTransaction(id string) *TransactionRecord
	FindTransactions(ids []string, id *string, account *string, contract *string, status *TransactionStatus, byType []string, ledgerToFrom *string, ledgerTypes []string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]TransactionRecord, error)
	FindUnconfirmedTransactions(height uint64) ([]TransactionRecord, error)
	InvalidateCompetingTransactions(requiredAuths []string, nonces []uint64) (int64, error)
	HasUnconfirmedWithNonce(requiredAuths []string, nonce uint64) (bool, error)
	// PruneExpiredUnconfirmed deletes UNCONFIRMED transactions whose expire_block has
	// already passed. FindUnconfirmedTransactions filters them out anyway, so deletion
	// is invisible to active reads. Local-only optimization.
	PruneExpiredUnconfirmed(ctx context.Context, currentHeight uint64) (int64, error)
	// PruneConfirmedOlderThan deletes terminal-state transaction records (CONFIRMED,
	// FAILED, INCLUDED) whose anchr_height < cutoff. Witness-only: full and archive
	// nodes serve historical tx queries via GraphQL. State engine and TSS never read
	// confirmed-tx records.
	PruneConfirmedOlderThan(ctx context.Context, cutoff uint64) (int64, error)
}
