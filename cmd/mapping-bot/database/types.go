package database

import (
	"encoding/json"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
)

type TxState string

const (
	TxStatePending   TxState = "pending"
	TxStateSent      TxState = "sent"
	TxStateConfirmed TxState = "confirmed"
)

var (
	ErrAddrExists       = errors.New("address key exists in datastore")
	ErrAddrNotFound     = errors.New("address not found")
	ErrTxNotFound       = errors.New("transaction not found")
	ErrTxExists         = errors.New("tx key exists in datastore")
	ErrFailedTxNotFound = errors.New("failed tx not found")
)

// AddressStore handles address mapping operations
type AddressStore struct {
	collection *mongo.Collection
}

// StateStore handles block height and transaction tracking
type StateStore struct {
	heightCollection *mongo.Collection
	txCollection     *mongo.Collection
}

// Database represents the complete database with organized sub-stores
type Database struct {
	client    *mongo.Client
	Addresses *AddressStore
	State     *StateStore
	FailedTxs *FailedTxStore
}

// AddressMapping represents a chain address to VSC instruction mapping document.
// Works for any UTXO chain (BTC, LTC, DASH).
type AddressMapping struct {
	ChainAddr   string    `bson:"_id"` // chain deposit address as primary key
	Instruction string    `bson:"instruction"`
	CreatedAt   time.Time `bson:"createdAt"`
}

// BlockHeight stores the last processed block height
type BlockHeight struct {
	ID        string     `bson:"_id"` // Always "current"
	Height    uint64     `bson:"height"`
	LockOwner string     `bson:"lockOwner,omitempty"`
	LockUntil *time.Time `bson:"lockUntil,omitempty"`
}

// Transaction represents a transaction in any state
type Transaction struct {
	TxID              string          `bson:"_id"`
	State             TxState         `bson:"state"`
	RawTx             []byte          `bson:"rawTx,omitempty"`
	TotalSignatures   uint64          `bson:"totalSignatures,omitempty"`
	CurrentSignatures uint64          `bson:"currentSignatures,omitempty"`
	CreatedAt         time.Time       `bson:"createdAt"`
	SentAt            *time.Time      `bson:"sentAt,omitempty"`
	SentAtHeight      uint64          `bson:"sentAtHeight,omitempty"`
	ConfirmedAt       *time.Time      `bson:"confirmedAt,omitempty"`
	Signatures        []SignatureSlot `bson:"signatures,omitempty"`

	// confirmSpend anti-stuck bookkeeping — populated only while a tx is "sent"
	// and its confirmSpend contract calls keep failing. Drives exponential
	// backoff between attempts and give-up after a bounded retry window.
	ConfirmAttempts       uint64     `bson:"confirmAttempts,omitempty"`
	FirstConfirmAttemptAt *time.Time `bson:"firstConfirmAttemptAt,omitempty"`
	NextConfirmAttemptAt  *time.Time `bson:"nextConfirmAttemptAt,omitempty"`
	ConfirmAbandoned      bool       `bson:"confirmAbandoned,omitempty"`
	LastConfirmError      string     `bson:"lastConfirmError,omitempty"`
}

// ConfirmSpendRetry carries the confirmSpend retry bookkeeping to persist after a
// failed attempt. The retry policy (backoff schedule, give-up window) lives in
// the mapper; this struct is just the computed result to store.
type ConfirmSpendRetry struct {
	Attempts       uint64
	FirstAttemptAt time.Time
	NextAttemptAt  time.Time
	Abandoned      bool
	LastError      string
}

// AbandonedConfirmSpend is a projection of a sent tx whose confirmSpend retries
// were exhausted. It deliberately omits rawTx/signatures so it is safe to expose
// via the health endpoint.
type AbandonedConfirmSpend struct {
	TxID             string     `bson:"_id"                   json:"txId"`
	ConfirmAttempts  uint64     `bson:"confirmAttempts"       json:"confirmAttempts"`
	LastConfirmError string     `bson:"lastConfirmError"      json:"lastConfirmError,omitempty"`
	FirstAttemptAt   *time.Time `bson:"firstConfirmAttemptAt" json:"firstAttemptAt,omitempty"`
	SentAtHeight     uint64     `bson:"sentAtHeight"          json:"sentAtHeight,omitempty"`
}

// SignatureSlot represents a signature slot in a transaction
type SignatureSlot struct {
	Index         uint64 `bson:"index"`
	SigHash       []byte `bson:"sigHash"`
	WitnessScript []byte `bson:"witnessScript"`
	Signature     []byte `bson:"signature,omitempty"` // omitempty keeps it null when empty
	IsBackup      bool   `bson:"isBackup,omitempty"`  // true when signed via HTTP (backup key path)
}

// FailedVscTx records a map or confirmSpend contract call that reached FAILED status on-chain.
// Persisted to MongoDB so failed txs survive bot restarts and can be retried via the HTTP API.
type FailedVscTx struct {
	TxId          string          `bson:"_id"                      json:"txId"`
	Action        string          `bson:"action"                   json:"action"`
	Payload       json.RawMessage `bson:"payload"                  json:"payload"`
	FailedAt      time.Time       `bson:"failedAt"                 json:"failedAt"`
	LastRetriedAt *time.Time      `bson:"lastRetriedAt,omitempty"  json:"lastRetriedAt,omitempty"`
}

// FailedTxStore handles failed VSC transaction persistence
type FailedTxStore struct {
	collection *mongo.Collection
}
