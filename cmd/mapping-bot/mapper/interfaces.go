package mapper

import (
	"context"
	"encoding/json"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"
)

// GraphQLFetcher abstracts contract state queries that currently go through
// the hasura GraphQL client. Implementations must be safe for concurrent use.
type GraphQLFetcher interface {
	FetchTxSpends(ctx context.Context) (map[string]*contractinterface.SigningData, error)
	FetchSignatures(ctx context.Context, msgHex []string) (map[string]database.SignatureUpdate, error)
	FetchLastHeight(ctx context.Context) (string, error)
	FetchPublicKeys(ctx context.Context) (primaryKeyHex []byte, backupKeyHex []byte, err error)
	FetchObservedTx(ctx context.Context, txId string, vout int) (bool, error)
	// FetchTransactionStatus queries the VSC node for the status of a transaction
	// by its Hive tx ID. Returns the status string (e.g. "INCLUDED", "CONFIRMED", "FAILED")
	// or an error if the transaction is not found or the query fails.
	FetchTransactionStatus(ctx context.Context, txId string) (string, error)
}

// ContractCaller abstracts Hive transaction broadcasting for contract calls.
type ContractCaller interface {
	CallContract(ctx context.Context, contractInput json.RawMessage, action string) (string, error)
}

// StateStore abstracts the database.StateStore operations used by Bot methods.
type StateStore interface {
	IncrementBlockHeight(ctx context.Context) (uint64, error)
	GetBlockHeight(ctx context.Context) (uint64, error)
	SetBlockHeight(ctx context.Context, height uint64) error

	AddPendingTransaction(ctx context.Context, txID string, rawTx []byte, unsignedHashes []contractinterface.UnsignedSigHash) error
	GetPendingTransaction(ctx context.Context, txID string) (*database.Transaction, error)
	GetAllPendingTransactions(ctx context.Context) ([]database.Transaction, error)
	GetAllPendingSigHashes(ctx context.Context) ([]string, error)
	UpdateSignatures(ctx context.Context, signatures map[string]database.SignatureUpdate) ([]*database.Transaction, error)

	IsTransactionProcessed(ctx context.Context, txID string) (bool, error)
	MarkTransactionSent(ctx context.Context, txID string) error
	MarkTransactionConfirmed(ctx context.Context, txID string) error
	GetSentTransactionIDs(ctx context.Context) ([]string, error)
}

// AddressStore abstracts the database.AddressStore operations used by Bot methods.
type AddressStore interface {
	GetInstruction(ctx context.Context, chainAddr string) (string, error)
	Insert(ctx context.Context, chainAddr, instruction string) error
}
