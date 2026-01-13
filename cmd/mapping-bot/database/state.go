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

// === Pending Transaction Methods ===

// AddPendingTransaction adds a new unsigned transaction to the pending queue
func (s *StateStore) AddPendingTransaction(
	ctx context.Context,
	txID string,
	rawTx string,
	unsignedHashes []UnsignedSigHash,
) error {
	signatures := make([]SignatureSlot, len(unsignedHashes))
	for i, uh := range unsignedHashes {
		signatures[i] = SignatureSlot{
			Index:         uh.Index,
			SigHash:       uh.SigHash,
			WitnessScript: uh.WitnessScript,
			Signature:     nil,
		}
	}

	doc := PendingTransaction{
		TxID:              txID,
		RawTx:             rawTx,
		TotalSignatures:   uint32(len(unsignedHashes)),
		CurrentSignatures: 0,
		CreatedAt:         time.Now().UTC(),
		Signatures:        signatures,
	}

	_, err := s.pendingTxCollection.InsertOne(ctx, doc)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrPendingTxExists
		}
		return fmt.Errorf("failed to insert pending transaction [txID:%s]: %w", txID, err)
	}
	return nil
}

// GetAllPendingSigHashes returns all signature hashes that are still unsigned
func (s *StateStore) GetAllPendingSigHashes(ctx context.Context) ([]string, error) {
	// Use aggregation to extract all unsigned sigHashes
	pipeline := mongo.Pipeline{
		// Unwind the signatures array
		{{Key: "$unwind", Value: "$signatures"}},
		// Filter for null signatures
		{{Key: "$match", Value: bson.M{"signatures.signature": nil}}},
		// Project just the sigHash
		{{Key: "$project", Value: bson.M{"_id": 0, "sigHash": "$signatures.sigHash"}}},
	}

	cursor, err := s.pendingTxCollection.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending sig hashes: %w", err)
	}
	defer cursor.Close(ctx)

	var results []struct {
		SigHash string `bson:"sigHash"`
	}
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode sig hashes: %w", err)
	}

	hashes := make([]string, len(results))
	for i, r := range results {
		hashes[i] = r.SigHash
	}
	return hashes, nil
}

// UpdateSignatures updates transactions with new signatures
// Returns the txIDs of transactions that are now fully signed
func (s *StateStore) UpdateSignatures(
	ctx context.Context,
	signatures map[string][]byte,
) ([]*PendingTransaction, error) {
	fullySignedTxIDs := make([]*PendingTransaction, 0)

	// For each signature, find and update the corresponding transaction
	for sigHash, sigBytes := range signatures {
		// Find the transaction containing this sigHash
		filter := bson.M{"signatures.sigHash": sigHash}

		var tx PendingTransaction
		err := s.pendingTxCollection.FindOne(ctx, filter).Decode(&tx)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				// Hash not found - might have already been processed
				continue
			}
			return nil, fmt.Errorf("failed to find transaction for sigHash %s: %w", sigHash, err)
		}

		// Find the signature index and update it
		for i := range tx.Signatures {
			if tx.Signatures[i].SigHash == sigHash && tx.Signatures[i].Signature == nil {
				// Update this specific signature slot
				update := bson.M{
					"$set": bson.M{
						fmt.Sprintf("signatures.%d.signature", i): sigBytes,
					},
					"$inc": bson.M{
						"currentSignatures": 1,
					},
				}

				_, err := s.pendingTxCollection.UpdateOne(ctx, bson.M{"_id": tx.TxID}, update)
				if err != nil {
					return nil, fmt.Errorf("failed to update signature for txID %s: %w", tx.TxID, err)
				}

				// Check if this transaction is now fully signed
				if tx.CurrentSignatures+1 >= tx.TotalSignatures {
					fullySignedTxIDs = append(fullySignedTxIDs, &tx)
				}
				break
			}
		}
	}

	return fullySignedTxIDs, nil
}

// GetPendingTransaction retrieves a pending transaction by ID
func (s *StateStore) GetPendingTransaction(ctx context.Context, txID string) (*PendingTransaction, error) {
	var tx PendingTransaction
	err := s.pendingTxCollection.FindOne(ctx, bson.M{"_id": txID}).Decode(&tx)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("transaction %s not found", txID)
		}
		return nil, fmt.Errorf("failed to get transaction %s: %w", txID, err)
	}
	return &tx, nil
}

// DeletePendingTransaction removes a transaction from the pending queue
func (s *StateStore) DeletePendingTransaction(ctx context.Context, txID string) error {
	result, err := s.pendingTxCollection.DeleteOne(ctx, bson.M{"_id": txID})
	if err != nil {
		return fmt.Errorf("failed to delete transaction %s: %w", txID, err)
	}
	if result.DeletedCount == 0 {
		return fmt.Errorf("transaction %s not found", txID)
	}
	return nil
}

// DeleteOldPendingTransactions removes pending transactions created before the specified duration ago
// Returns the number of transactions deleted
func (s *StateStore) DeleteOldPendingTransactions(ctx context.Context, age time.Duration) (int64, error) {
	cutoffTime := time.Now().UTC().Add(-age)
	filter := bson.M{"createdAt": bson.M{"$lt": cutoffTime}}

	result, err := s.pendingTxCollection.DeleteMany(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("failed to delete old pending transactions: %w", err)
	}
	return result.DeletedCount, nil
}

// ClearAllPendingTransactions removes all pending transactions
func (s *StateStore) ClearAllPendingTransactions(ctx context.Context) error {
	_, err := s.pendingTxCollection.DeleteMany(ctx, bson.M{})
	return err
}

// GetAllPendingTransactions retrieves all pending transactions
func (s *StateStore) GetAllPendingTransactions(ctx context.Context) ([]PendingTransaction, error) {
	cursor, err := s.pendingTxCollection.Find(ctx, bson.M{})
	if err != nil {
		return nil, fmt.Errorf("failed to get all pending transactions: %w", err)
	}
	defer cursor.Close(ctx)

	var txs []PendingTransaction
	if err := cursor.All(ctx, &txs); err != nil {
		return nil, fmt.Errorf("failed to decode pending transactions: %w", err)
	}
	return txs, nil
}
