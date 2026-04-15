package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// RecordFailed upserts a failed VSC transaction record.
// If a record with the same TxId already exists, it is overwritten.
func (s *FailedTxStore) RecordFailed(ctx context.Context, txId, action string, payload json.RawMessage) error {
	doc := FailedVscTx{
		TxId:     txId,
		Action:   action,
		Payload:  payload,
		FailedAt: time.Now().UTC(),
	}
	opts := options.Replace().SetUpsert(true)
	_, err := s.collection.ReplaceOne(ctx, bson.M{"_id": txId}, doc, opts)
	if err != nil {
		return fmt.Errorf("failed to record failed tx [txId:%s]: %w", txId, err)
	}
	return nil
}

// GetAll returns all persisted failed VSC transactions ordered by failedAt descending.
func (s *FailedTxStore) GetAll(ctx context.Context) ([]FailedVscTx, error) {
	cursor, err := s.collection.Find(
		ctx,
		bson.M{},
		options.Find().SetSort(bson.D{{Key: "failedAt", Value: -1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list failed txs: %w", err)
	}
	defer cursor.Close(ctx)

	var results []FailedVscTx
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode failed txs: %w", err)
	}
	return results, nil
}

// GetOne returns a single failed VSC transaction by its ID.
// Returns ErrFailedTxNotFound if no record exists.
func (s *FailedTxStore) GetOne(ctx context.Context, txId string) (*FailedVscTx, error) {
	var result FailedVscTx
	err := s.collection.FindOne(ctx, bson.M{"_id": txId}).Decode(&result)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrFailedTxNotFound
		}
		return nil, fmt.Errorf("failed to get failed tx [txId:%s]: %w", txId, err)
	}
	return &result, nil
}

// TryMarkRetrying atomically sets lastRetriedAt to now if:
//   - no lastRetriedAt is set, OR
//   - lastRetriedAt is older than throttle
//
// Returns true if the update was applied (retry is allowed), false if throttled.
func (s *FailedTxStore) TryMarkRetrying(ctx context.Context, txId string, throttle time.Duration) (bool, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-throttle)

	filter := bson.M{
		"_id": txId,
		"$or": bson.A{
			bson.M{"lastRetriedAt": bson.M{"$exists": false}},
			bson.M{"lastRetriedAt": nil},
			bson.M{"lastRetriedAt": bson.M{"$lt": cutoff}},
		},
	}
	update := bson.M{"$set": bson.M{"lastRetriedAt": now}}

	result, err := s.collection.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, fmt.Errorf("failed to mark retrying [txId:%s]: %w", txId, err)
	}
	return result.MatchedCount > 0, nil
}

// Delete removes a failed VSC transaction record by its ID.
func (s *FailedTxStore) Delete(ctx context.Context, txId string) error {
	result, err := s.collection.DeleteOne(ctx, bson.M{"_id": txId})
	if err != nil {
		return fmt.Errorf("failed to delete failed tx [txId:%s]: %w", txId, err)
	}
	if result.DeletedCount == 0 {
		return ErrFailedTxNotFound
	}
	return nil
}
