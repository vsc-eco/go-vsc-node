package database

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

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
	txCollection := db.Collection("transactions")

	// Create index on createdAt for address mappings
	addrIndexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "createdAt", Value: 1}},
		Options: options.Index().SetName("createdAt_idx"),
	}
	if _, err := addrCollection.Indexes().CreateOne(ctx, addrIndexModel); err != nil {
		return nil, fmt.Errorf("failed to create address index: %w", err)
	}

	// Index on state for filtering pending/sent/confirmed
	txStateIndexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "state", Value: 1}},
		Options: options.Index().SetName("state_idx"),
	}
	if _, err := txCollection.Indexes().CreateOne(ctx, txStateIndexModel); err != nil {
		return nil, fmt.Errorf("failed to create tx state index: %w", err)
	}

	// Index on signatures.sigHash for fast hash lookups during signing
	txSigHashIndexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "signatures.sigHash", Value: 1}},
		Options: options.Index().SetName("sigHash_idx"),
	}
	if _, err := txCollection.Indexes().CreateOne(ctx, txSigHashIndexModel); err != nil {
		return nil, fmt.Errorf("failed to create tx sigHash index: %w", err)
	}

	// Index on createdAt for cleanup of old pending transactions
	txCreatedAtIndexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "createdAt", Value: 1}},
		Options: options.Index().SetName("createdAt_idx"),
	}
	if _, err := txCollection.Indexes().CreateOne(ctx, txCreatedAtIndexModel); err != nil {
		return nil, fmt.Errorf("failed to create tx createdAt index: %w", err)
	}

	// Index on sentAt for cleanup of old sent transactions
	txSentAtIndexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "sentAt", Value: 1}},
		Options: options.Index().SetName("sentAt_idx"),
	}
	if _, err := txCollection.Indexes().CreateOne(ctx, txSentAtIndexModel); err != nil {
		return nil, fmt.Errorf("failed to create tx sentAt index: %w", err)
	}

	return &Database{
		client: client,
		Addresses: &AddressStore{
			collection: addrCollection,
		},
		State: &StateStore{
			heightCollection: heightCollection,
			txCollection:     txCollection,
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
	return nil
}

// DropDatabase deletes the entire database
func (d *Database) DropDatabase(ctx context.Context) error {
	dbName := d.Addresses.collection.Database().Name()
	return d.client.Database(dbName).Drop(ctx)
}
