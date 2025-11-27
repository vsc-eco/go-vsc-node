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
	ErrAddrExists   = errors.New("key exists in datastore")
	ErrAddrNotFound = errors.New("address not found")
)

// AddressMapping represents a BTC to VSC address mapping document
type AddressMapping struct {
	BtcAddr     string    `bson:"_id"` // Use btcAddr as primary key
	Instruction string    `bson:"instruction"`
	CreatedAt   time.Time `bson:"createdAt"`
}

type MappingBotDatabase struct {
	client     *mongo.Client
	collection *mongo.Collection
}

// New creates a new MongoDB-backed database
func New(ctx context.Context, connString, dbName, collectionName string) (*MappingBotDatabase, error) {
	// Create client
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(connString))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}

	// Ping to verify connection
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping MongoDB: %w", err)
	}

	collection := client.Database(dbName).Collection(collectionName)

	// Create unique index on btcAddr (which is _id, so automatically unique)
	// This is just for documentation - _id is always unique
	// But you might want indexes on other fields later
	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "createdAt", Value: 1}},
		Options: options.Index().SetName("createdAt_idx"),
	}

	_, err = collection.Indexes().CreateOne(ctx, indexModel)
	if err != nil {
		return nil, fmt.Errorf("failed to create index: %w", err)
	}

	mdb := &MappingBotDatabase{
		client:     client,
		collection: collection,
	}

	return mdb, nil
}

// Close cleanly disconnects from MongoDB
func (m *MappingBotDatabase) Close(ctx context.Context) error {
	return m.client.Disconnect(ctx)
}

// DropCollection deletes the entire collection and all its data
// Use with caution! This is irreversible.
func (m *MappingBotDatabase) DropCollection(ctx context.Context) error {
	return m.collection.Drop(ctx)
}

// DropDatabase deletes the entire database and all its collections
// Use with caution! This is irreversible.
// Useful for test cleanup.
func (m *MappingBotDatabase) DropDatabase(ctx context.Context) error {
	dbName := m.collection.Database().Name()
	err := m.client.Database(dbName).Drop(ctx)
	if err != nil {
		return fmt.Errorf("failed to drop database %s: %w", dbName, err)
	}
	return nil
}

// ClearAllMappings removes all documents but keeps the collection and indexes
func (m *MappingBotDatabase) ClearAllMappings(ctx context.Context) error {
	_, err := m.collection.DeleteMany(ctx, bson.M{})
	if err != nil {
		return fmt.Errorf("failed to clear mappings: %w", err)
	}
	return nil
}

// DeleteOlderThan removes all mappings created before the specified duration ago
// Returns the number of documents deleted
func (m *MappingBotDatabase) DeleteOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	cutoffTime := time.Now().UTC().Add(-age)

	filter := bson.M{
		"createdAt": bson.M{
			"$lt": cutoffTime,
		},
	}

	result, err := m.collection.DeleteMany(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("failed to delete old mappings: %w", err)
	}

	return result.DeletedCount, nil
}

// InsertAddressMap stores a BTC to VSC address mapping
// Returns ErrAddrExists if the btcAddr already exists
func (m *MappingBotDatabase) InsertAddressMap(
	ctx context.Context,
	btcAddr, instruction string,
) error {
	doc := AddressMapping{
		BtcAddr:     btcAddr,
		Instruction: instruction,
		CreatedAt:   time.Now().UTC(),
	}

	_, err := m.collection.InsertOne(ctx, doc)
	if err != nil {
		// Check if it's a duplicate key error
		if mongo.IsDuplicateKeyError(err) {
			return ErrAddrExists
		}
		return fmt.Errorf("failed to insert address mapping [btcAddr:%s]: %w", btcAddr, err)
	}

	return nil
}

// GetInstruction retrieves the VSC address for a given BTC address
// Returns ErrAddrNotFound if the btcAddr doesn't exist
func (m *MappingBotDatabase) GetInstruction(
	ctx context.Context,
	btcAddr string,
) (string, error) {
	var result AddressMapping

	err := m.collection.FindOne(ctx, bson.M{"_id": btcAddr}).Decode(&result)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return "", ErrAddrNotFound
		}
		return "", fmt.Errorf("failed to get vscAddress [btcAddr: %s]: %w", btcAddr, err)
	}

	return result.Instruction, nil
}

// Example additional query methods you might want later:

// GetMappingsByDateRange retrieves all mappings created within a date range
func (m *MappingBotDatabase) GetMappingsByDateRange(
	ctx context.Context,
	start, end time.Time,
) ([]AddressMapping, error) {
	filter := bson.M{
		"createdAt": bson.M{
			"$gte": start,
			"$lte": end,
		},
	}

	cursor, err := m.collection.Find(ctx, filter)
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
func (m *MappingBotDatabase) SearchByVscAddr(
	ctx context.Context,
	vscAddrPattern string,
) ([]AddressMapping, error) {
	filter := bson.M{
		"vscAddr": bson.M{
			"$regex":   vscAddrPattern,
			"$options": "i", // case-insensitive
		},
	}

	cursor, err := m.collection.Find(ctx, filter)
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
