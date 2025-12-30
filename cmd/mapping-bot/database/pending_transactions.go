package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

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
