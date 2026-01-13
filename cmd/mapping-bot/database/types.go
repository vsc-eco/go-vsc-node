package database

import (
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
)

var (
	ErrAddrExists      = errors.New("address key exists in datastore")
	ErrAddrNotFound    = errors.New("address not found")
	ErrTxNotFound      = errors.New("transaction not found")
	ErrPendingTxExists = errors.New("pending tx key exists in datastore")
)

// AddressStore handles address mapping operations
type AddressStore struct {
	collection *mongo.Collection
}

// StateStore handles block height and transaction tracking
type StateStore struct {
	heightCollection    *mongo.Collection
	txCollection        *mongo.Collection
	pendingTxCollection *mongo.Collection
}

// Database represents the complete database with organized sub-stores
type Database struct {
	client    *mongo.Client
	Addresses *AddressStore
	State     *StateStore
}

// AddressMapping represents a BTC to VSC address mapping document
type AddressMapping struct {
	BtcAddr     string    `bson:"_id"` // Use btcAddr as primary key
	Instruction string    `bson:"instruction"`
	CreatedAt   time.Time `bson:"createdAt"`
}

// BlockHeight stores the last processed block height
type BlockHeight struct {
	ID     string `bson:"_id"` // Always "current"
	Height uint32 `bson:"height"`
}

// SentTransaction stores a transaction ID that has been sent
type SentTransaction struct {
	TxID   string    `bson:"_id"` // Transaction ID as primary key
	SentAt time.Time `bson:"sentAt"`
}

// PendingTransaction represents a transaction awaiting signatures
type PendingTransaction struct {
	TxID              string          `bson:"_id"`
	RawTx             string          `bson:"rawTx"`
	TotalSignatures   uint32          `bson:"totalSignatures"`
	CurrentSignatures uint32          `bson:"currentSignatures"`
	CreatedAt         time.Time       `bson:"createdAt"`
	Signatures        []SignatureSlot `bson:"signatures"`
}

// SignatureSlot represents a signature slot in a transaction
type SignatureSlot struct {
	Index         uint32 `bson:"index"`
	SigHash       string `bson:"sigHash"`
	WitnessScript string `bson:"witnessScript"`
	Signature     []byte `bson:"signature,omitempty"` // omitempty keeps it null when empty
}

// UnsignedSigHash is a helper struct for adding transactions
type UnsignedSigHash struct {
	Index         uint32 `json:"index"`
	SigHash       string `json:"sig_hash"`
	WitnessScript string `json:"witness_script"`
}
