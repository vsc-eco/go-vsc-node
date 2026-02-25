package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

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
