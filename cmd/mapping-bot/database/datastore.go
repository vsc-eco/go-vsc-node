package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	ErrAddrExists      = errors.New("address key exists in datastore")
	ErrAddrNotFound    = errors.New("address not found")
	ErrTxNotFound      = errors.New("transaction not found")
	ErrPendingTxExists = errors.New("pending tx key exists in datastore")
)

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

// New creates a new MongoDB-backed database with all collections
func New(ctx context.Context, connString, dbName string) (*Database, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(connString))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping MongoDB: %w", err)
	}

	db := client.Database(dbName)
	addrCollection := db.Collection("address_mappings")
	heightCollection := db.Collection("block_height")
	txCollection := db.Collection("sent_transactions")
	pendingTxCollection := db.Collection("pending_transactions")

	// Create index on createdAt for address mappings
	addrIndexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "createdAt", Value: 1}},
		Options: options.Index().SetName("createdAt_idx"),
	}
	if _, err := addrCollection.Indexes().CreateOne(ctx, addrIndexModel); err != nil {
		return nil, fmt.Errorf("failed to create address index: %w", err)
	}

	// Create index on sentAt for transactions
	txIndexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "sentAt", Value: 1}},
		Options: options.Index().SetName("sentAt_idx"),
	}
	if _, err := txCollection.Indexes().CreateOne(ctx, txIndexModel); err != nil {
		return nil, fmt.Errorf("failed to create transaction index: %w", err)
	}

	// Create index on signatures.sigHash for fast hash lookups
	pendingSigHashIndexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "signatures.sigHash", Value: 1}},
		Options: options.Index().SetName("sigHash_idx"),
	}
	if _, err := pendingTxCollection.Indexes().CreateOne(ctx, pendingSigHashIndexModel); err != nil {
		return nil, fmt.Errorf("failed to create pending tx sigHash index: %w", err)
	}

	// Create index on createdAt for cleanup of old pending transactions
	pendingCreatedAtIndexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "createdAt", Value: 1}},
		Options: options.Index().SetName("pending_createdAt_idx"),
	}
	if _, err := pendingTxCollection.Indexes().CreateOne(ctx, pendingCreatedAtIndexModel); err != nil {
		return nil, fmt.Errorf("failed to create pending tx createdAt index: %w", err)
	}

	return &Database{
		client: client,
		Addresses: &AddressStore{
			collection: addrCollection,
		},
		State: &StateStore{
			heightCollection:    heightCollection,
			txCollection:        txCollection,
			pendingTxCollection: pendingTxCollection,
		},
	}, nil
}

// Close cleanly disconnects from MongoDB
func (d *Database) Close(ctx context.Context) error {
	return d.client.Disconnect(ctx)
}

// DropAllCollections deletes all collections
func (d *Database) DropAllCollections(ctx context.Context) error {
	if err := d.Addresses.collection.Drop(ctx); err != nil {
		return fmt.Errorf("failed to drop address collection: %w", err)
	}
	if err := d.State.heightCollection.Drop(ctx); err != nil {
		return fmt.Errorf("failed to drop height collection: %w", err)
	}
	if err := d.State.txCollection.Drop(ctx); err != nil {
		return fmt.Errorf("failed to drop tx collection: %w", err)
	}
	if err := d.State.pendingTxCollection.Drop(ctx); err != nil {
		return fmt.Errorf("failed to drop pending tx collection: %w", err)
	}
	return nil
}

// DropDatabase deletes the entire database
func (d *Database) DropDatabase(ctx context.Context) error {
	dbName := d.Addresses.collection.Database().Name()
	return d.client.Database(dbName).Drop(ctx)
}

// === AddressStore Methods ===

// ClearAll removes all address mapping documents
func (a *AddressStore) ClearAll(ctx context.Context) error {
	_, err := a.collection.DeleteMany(ctx, bson.M{})
	return err
}

// DeleteOlderThan removes mappings created before the specified duration ago
func (a *AddressStore) DeleteOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	cutoffTime := time.Now().UTC().Add(-age)
	filter := bson.M{"createdAt": bson.M{"$lt": cutoffTime}}
	result, err := a.collection.DeleteMany(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("failed to delete old mappings: %w", err)
	}
	return result.DeletedCount, nil
}

// Insert stores a BTC to VSC address mapping
func (a *AddressStore) Insert(ctx context.Context, btcAddr, instruction string) error {
	doc := AddressMapping{
		BtcAddr:     btcAddr,
		Instruction: instruction,
		CreatedAt:   time.Now().UTC(),
	}

	_, err := a.collection.InsertOne(ctx, doc)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrAddrExists
		}
		return fmt.Errorf("failed to insert address mapping [btcAddr:%s]: %w", btcAddr, err)
	}
	return nil
}

// GetInstruction retrieves the instruction for a given BTC address
func (a *AddressStore) GetInstruction(ctx context.Context, btcAddr string) (string, error) {
	var result AddressMapping
	err := a.collection.FindOne(ctx, bson.M{"_id": btcAddr}).Decode(&result)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return "", ErrAddrNotFound
		}
		return "", fmt.Errorf("failed to get instruction [btcAddr: %s]: %w", btcAddr, err)
	}
	return result.Instruction, nil
}

// GetByDateRange retrieves mappings created within a date range
func (a *AddressStore) GetByDateRange(ctx context.Context, start, end time.Time) ([]AddressMapping, error) {
	filter := bson.M{"createdAt": bson.M{"$gte": start, "$lte": end}}
	cursor, err := a.collection.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to query by date range: %w", err)
	}
	defer cursor.Close(ctx)

	var results []AddressMapping
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode results: %w", err)
	}
	return results, nil
}

// SearchByVscAddr finds mappings where vscAddr contains the given substring
func (a *AddressStore) SearchByVscAddr(ctx context.Context, vscAddrPattern string) ([]AddressMapping, error) {
	filter := bson.M{"vscAddr": bson.M{"$regex": vscAddrPattern, "$options": "i"}}
	cursor, err := a.collection.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to search by vscAddr: %w", err)
	}
	defer cursor.Close(ctx)

	var results []AddressMapping
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode results: %w", err)
	}
	return results, nil
}

// === StateStore Methods ===

// SetBlockHeight sets the last processed block height
func (s *StateStore) SetBlockHeight(ctx context.Context, height uint32) error {
	doc := BlockHeight{
		ID:     "current",
		Height: height,
	}

	opts := options.Replace().SetUpsert(true)
	_, err := s.heightCollection.ReplaceOne(ctx, bson.M{"_id": "current"}, doc, opts)
	if err != nil {
		return fmt.Errorf("failed to set block height: %w", err)
	}
	return nil
}

// GetBlockHeight retrieves the last processed block height
// Returns 0 if no height has been set yet
func (s *StateStore) GetBlockHeight(ctx context.Context) (uint32, error) {
	var result BlockHeight
	err := s.heightCollection.FindOne(ctx, bson.M{"_id": "current"}).Decode(&result)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil // Return 0 if no height has been set
		}
		return 0, fmt.Errorf("failed to get block height: %w", err)
	}
	return result.Height, nil
}

// IncrementBlockHeight atomically increments the block height by 1
// Returns the new height after increment
// If no height exists, initializes to 1
func (s *StateStore) IncrementBlockHeight(ctx context.Context) (uint32, error) {
	filter := bson.M{"_id": "current"}
	update := bson.M{
		"$inc": bson.M{"height": 1},
	}

	opts := options.FindOneAndUpdate().
		SetUpsert(true).
		SetReturnDocument(options.After)

	var result BlockHeight
	err := s.heightCollection.FindOneAndUpdate(ctx, filter, update, opts).Decode(&result)
	if err != nil {
		return 0, fmt.Errorf("failed to increment block height: %w", err)
	}

	return result.Height, nil
}

// MarkTransactionSent marks a transaction ID as sent
// Returns ErrAddrExists if the transaction was already marked as sent
func (s *StateStore) MarkTransactionSent(ctx context.Context, txID string) error {
	err := s.DeletePendingTransaction(ctx, txID)
	if err != nil {
		return err
	}

	doc := SentTransaction{
		TxID:   txID,
		SentAt: time.Now().UTC(),
	}

	_, err = s.txCollection.InsertOne(ctx, doc)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrAddrExists // Reusing this error, or create ErrTxExists
		}
		return fmt.Errorf("failed to mark transaction as sent [txID:%s]: %w", txID, err)
	}
	return nil
}

// IsTransactionSent checks if a transaction ID has been marked as sent
func (s *StateStore) IsTransactionSent(ctx context.Context, txID string) (bool, error) {
	var result SentTransaction
	err := s.txCollection.FindOne(ctx, bson.M{"_id": txID}).Decode(&result)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check transaction [txID:%s]: %w", txID, err)
	}
	return true, nil
}

// DeleteTransaction removes a transaction from the sent list
func (s *StateStore) DeleteTransaction(ctx context.Context, txID string) error {
	result, err := s.txCollection.DeleteOne(ctx, bson.M{"_id": txID})
	if err != nil {
		return fmt.Errorf("failed to delete transaction [txID:%s]: %w", txID, err)
	}
	if result.DeletedCount == 0 {
		return ErrTxNotFound
	}
	return nil
}

// DeleteOldTransactions removes transactions sent before the specified duration ago
func (s *StateStore) DeleteOldTransactions(ctx context.Context, age time.Duration) (int64, error) {
	cutoffTime := time.Now().UTC().Add(-age)
	filter := bson.M{"sentAt": bson.M{"$lt": cutoffTime}}
	result, err := s.txCollection.DeleteMany(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("failed to delete old transactions: %w", err)
	}
	return result.DeletedCount, nil
}

// GetAllTransactions retrieves all sent transaction IDs
func (s *StateStore) GetAllTransactions(ctx context.Context) ([]string, error) {
	cursor, err := s.txCollection.Find(ctx, bson.M{})
	if err != nil {
		return nil, fmt.Errorf("failed to get sent transactions: %w", err)
	}
	defer cursor.Close(ctx)

	var results []SentTransaction
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode results: %w", err)
	}

	txIDs := make([]string, len(results))
	for i, tx := range results {
		txIDs[i] = tx.TxID
	}
	return txIDs, nil
}

// ClearAllTransactions removes all sent transaction records
func (s *StateStore) ClearAllTransactions(ctx context.Context) error {
	_, err := s.txCollection.DeleteMany(ctx, bson.M{})
	return err
}
