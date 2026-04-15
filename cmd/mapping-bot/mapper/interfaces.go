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
	// by its tx ID (Hive tx ID for custom_json-submitted, or L2 CID for
	// submitTransactionV1-submitted). Returns the status string (e.g. "INCLUDED",
	// "CONFIRMED", "FAILED") or an error if the transaction is not found.
	FetchTransactionStatus(ctx context.Context, txId string) (string, error)
	// FetchAccountNonce returns the next unused nonce for the given VSC account
	// (a did:* or hive:* identifier). Used by the L2 submission path.
	FetchAccountNonce(ctx context.Context, account string) (uint64, error)
	// SubmitTransactionV1 submits a signed VSC L2 transaction (CBOR-encoded tx
	// and signature, base64url-encoded) and returns the resulting CID tx ID.
	SubmitTransactionV1(ctx context.Context, txB64, sigB64 string) (string, error)
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
	AdvanceBlockHeightIfCurrent(ctx context.Context, current, next uint64) (bool, error)

	AddPendingTransaction(ctx context.Context, txID string, rawTx []byte, unsignedHashes []contractinterface.UnsignedSigHash) error
	GetPendingTransaction(ctx context.Context, txID string) (*database.Transaction, error)
	GetAllPendingTransactions(ctx context.Context) ([]database.Transaction, error)
	GetAllPendingSigHashes(ctx context.Context) ([]string, error)
	GetFullySignedPendingTransactions(ctx context.Context) ([]*database.Transaction, error)
	UpdateSignatures(ctx context.Context, signatures map[string]database.SignatureUpdate) ([]*database.Transaction, error)

	IsTransactionProcessed(ctx context.Context, txID string) (bool, error)
	MarkTransactionSent(ctx context.Context, txID string, blockHeight uint64) error
	MarkTransactionConfirmed(ctx context.Context, txID string) error
	GetSentTransactionIDs(ctx context.Context) ([]string, error)
	GetSentTransactions(ctx context.Context) ([]database.Transaction, error)
}

// AddressStore abstracts the database.AddressStore operations used by Bot methods.
type AddressStore interface {
	GetInstruction(ctx context.Context, chainAddr string) (string, error)
	Insert(ctx context.Context, chainAddr, instruction string) error
}
