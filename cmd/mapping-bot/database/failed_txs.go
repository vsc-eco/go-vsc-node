package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ipfs/go-cid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// IsValidTxID reports whether txId is a well-formed IPFS CID.
// The mapping bot only stores IDs returned by SubmitTransactionV1, which are
// always VSC L2 CIDs. Hive tx IDs (64-char hex) are never stored here.
func IsValidTxID(txId string) bool {
	_, err := cid.Decode(txId)
	return err == nil
}

// validateTxID parses txId as an IPFS CID and returns its canonical string
// representation, breaking the CodeQL taint chain analogous to the
// hex.DecodeString/EncodeToString round-trip used in state.go.
func validateTxID(txId string) (string, error) {
	c, err := cid.Decode(txId)
	if err != nil {
		return "", fmt.Errorf("invalid txId: %w", err)
	}
	return c.String(), nil
}

// RecordFailed upserts a failed VSC transaction record.
// If a record with the same TxId already exists, it is overwritten.
func (s *FailedTxStore) RecordFailed(ctx context.Context, txId, action string, payload json.RawMessage) error {
	safeTxId, err := validateTxID(txId)
	if err != nil {
		return fmt.Errorf("failed to record failed tx [txId:%s]: %w", txId, err)
	}
	doc := FailedVscTx{
		TxId:     safeTxId,
		Action:   action,
		Payload:  payload,
		FailedAt: time.Now().UTC(),
	}
	opts := options.Replace().SetUpsert(true)
	_, err = s.collection.ReplaceOne(ctx, bson.M{"_id": safeTxId}, doc, opts)
	if err != nil {
		return fmt.Errorf("failed to record failed tx [txId:%s]: %w", txId, err)
	}
	return nil
}

// maxFailedTxResults caps the number of records returned by GetAll to prevent
// unbounded memory use in the health endpoint for unusually large failure sets.
const maxFailedTxResults = 100

// GetAll returns the most recent persisted failed VSC transactions, newest first,
// up to maxFailedTxResults records.
func (s *FailedTxStore) GetAll(ctx context.Context) ([]FailedVscTx, error) {
	cursor, err := s.collection.Find(
		ctx,
		bson.M{},
		options.Find().
			SetSort(bson.D{{Key: "failedAt", Value: -1}}).
			SetLimit(maxFailedTxResults),
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
	safeTxId, err := validateTxID(txId)
	if err != nil {
		return nil, fmt.Errorf("failed to get failed tx [txId:%s]: %w", txId, err)
	}
	var result FailedVscTx
	err = s.collection.FindOne(ctx, bson.M{"_id": safeTxId}).Decode(&result)
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
	safeTxId, err := validateTxID(txId)
	if err != nil {
		return false, fmt.Errorf("failed to mark retrying [txId:%s]: %w", txId, err)
	}
	now := time.Now().UTC()
	cutoff := now.Add(-throttle)

	filter := bson.M{
		"_id": safeTxId,
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
	safeTxId, err := validateTxID(txId)
	if err != nil {
		return fmt.Errorf("failed to delete failed tx [txId:%s]: %w", txId, err)
	}
	result, err := s.collection.DeleteOne(ctx, bson.M{"_id": safeTxId})
	if err != nil {
		return fmt.Errorf("failed to delete failed tx [txId:%s]: %w", txId, err)
	}
	if result.DeletedCount == 0 {
		return ErrFailedTxNotFound
	}
	return nil
}
