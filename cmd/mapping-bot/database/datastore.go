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
