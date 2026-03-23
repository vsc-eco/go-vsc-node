package database

import (
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
	ErrAddrExists   = errors.New("address key exists in datastore")
	ErrAddrNotFound = errors.New("address not found")
	ErrTxNotFound   = errors.New("transaction not found")
	ErrTxExists     = errors.New("tx key exists in datastore")
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
	ConfirmedAt       *time.Time      `bson:"confirmedAt,omitempty"`
	Signatures        []SignatureSlot `bson:"signatures,omitempty"`
}

// SignatureSlot represents a signature slot in a transaction
type SignatureSlot struct {
	Index         uint64 `bson:"index"`
	SigHash       []byte `bson:"sigHash"`
	WitnessScript []byte `bson:"witnessScript"`
	Signature     []byte `bson:"signature,omitempty"` // omitempty keeps it null when empty
	IsBackup      bool   `bson:"isBackup,omitempty"`  // true when signed via HTTP (backup key path)
}
