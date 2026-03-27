package database

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// === StateStore Methods ===

// SetBlockHeight sets the last processed block height
func (s *StateStore) SetBlockHeight(ctx context.Context, height uint64) error {
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
func (s *StateStore) GetBlockHeight(ctx context.Context) (uint64, error) {
	var result BlockHeight
	err := s.heightCollection.FindOne(ctx, bson.M{"_id": "current"}).Decode(&result)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get block height: %w", err)
	}
	return result.Height, nil
}

// IncrementBlockHeight atomically increments the block height by 1
// Returns the new height after increment
// If no height exists, initializes to 1
func (s *StateStore) IncrementBlockHeight(ctx context.Context) (uint64, error) {
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

// AdvanceBlockHeightIfCurrent atomically advances height from current to next.
// Returns true if the advance was applied, false if current did not match.
func (s *StateStore) AdvanceBlockHeightIfCurrent(ctx context.Context, current, next uint64) (bool, error) {
	result, err := s.heightCollection.UpdateOne(
		ctx,
		bson.M{"_id": "current", "height": current},
		bson.M{"$set": bson.M{"height": next}},
	)
	if err != nil {
		return false, fmt.Errorf("failed to advance block height from %d to %d: %w", current, next, err)
	}
	return result.MatchedCount > 0, nil
}

// TryAcquireBlockLease attempts to lease processing rights for a specific height.
// Returns true if the lease was acquired by owner.
func (s *StateStore) TryAcquireBlockLease(ctx context.Context, height uint64, owner string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	until := now.Add(ttl)
	filter := bson.M{
		"_id":    "current",
		"height": height,
		"$or": bson.A{
			bson.M{"lockUntil": bson.M{"$exists": false}},
			bson.M{"lockUntil": bson.M{"$lt": now}},
			bson.M{"lockOwner": owner},
		},
	}
	update := bson.M{
		"$set": bson.M{
			"lockOwner": owner,
			"lockUntil": until,
		},
	}
	result, err := s.heightCollection.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, fmt.Errorf("failed to acquire block lease [height:%d owner:%s]: %w", height, owner, err)
	}
	return result.MatchedCount > 0, nil
}

// ReleaseBlockLease releases a previously acquired block lease.
func (s *StateStore) ReleaseBlockLease(ctx context.Context, height uint64, owner string) error {
	_, err := s.heightCollection.UpdateOne(
		ctx,
		bson.M{"_id": "current", "height": height, "lockOwner": owner},
		bson.M{"$unset": bson.M{"lockOwner": "", "lockUntil": ""}},
	)
	if err != nil {
		return fmt.Errorf("failed to release block lease [height:%d owner:%s]: %w", height, owner, err)
	}
	return nil
}

// === Transaction Methods ===

// AddPendingTransaction adds a new unsigned transaction
func (s *StateStore) AddPendingTransaction(
	ctx context.Context,
	txID string,
	rawTx []byte,
	unsignedHashes []contractinterface.UnsignedSigHash,
) error {
	signatures := make([]SignatureSlot, len(unsignedHashes))
	for i, uh := range unsignedHashes {
		signatures[i] = SignatureSlot{
			Index:         uint64(uh.Index),
			SigHash:       uh.SigHash,
			WitnessScript: uh.WitnessScript,
			Signature:     nil,
		}
	}

	doc := Transaction{
		TxID:              txID,
		State:             TxStatePending,
		RawTx:             rawTx,
		TotalSignatures:   uint64(len(unsignedHashes)),
		CurrentSignatures: 0,
		CreatedAt:         time.Now().UTC(),
		Signatures:        signatures,
	}

	_, err := s.txCollection.InsertOne(ctx, doc)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrTxExists
		}
		return fmt.Errorf("failed to insert pending transaction [txID:%s]: %w", txID, err)
	}
	return nil
}

// GetPendingTransaction retrieves a pending transaction by ID
func (s *StateStore) GetPendingTransaction(ctx context.Context, txID string) (*Transaction, error) {
	if len(txID) != 64 {
		return nil, fmt.Errorf("invalid txID length %d: %w", len(txID), ErrTxNotFound)
	}
	if _, err := hex.DecodeString(txID); err != nil {
		return nil, fmt.Errorf("invalid txID hex: %w", ErrTxNotFound)
	}

	var tx Transaction
	err := s.txCollection.FindOne(ctx, bson.M{"_id": txID, "state": TxStatePending}).Decode(&tx)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("transaction %s not found: %w", txID, ErrTxNotFound)
		}
		return nil, fmt.Errorf("failed to get transaction %s: %w", txID, err)
	}
	return &tx, nil
}

// GetAllPendingTransactions retrieves all pending transactions
func (s *StateStore) GetAllPendingTransactions(ctx context.Context) ([]Transaction, error) {
	cursor, err := s.txCollection.Find(ctx, bson.M{"state": TxStatePending})
	if err != nil {
		return nil, fmt.Errorf("failed to get all pending transactions: %w", err)
	}
	defer cursor.Close(ctx)

	var txs []Transaction
	if err := cursor.All(ctx, &txs); err != nil {
		return nil, fmt.Errorf("failed to decode pending transactions: %w", err)
	}
	return txs, nil
}

// GetAllPendingSigHashes returns all signature hashes that are still unsigned
func (s *StateStore) GetAllPendingSigHashes(ctx context.Context) ([]string, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"state": TxStatePending}}},
		{{Key: "$unwind", Value: "$signatures"}},
		{{Key: "$match", Value: bson.M{"signatures.signature": nil}}},
		{{Key: "$project", Value: bson.M{"_id": 0, "sigHash": "$signatures.sigHash"}}},
	}

	cursor, err := s.txCollection.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending sig hashes: %w", err)
	}
	defer cursor.Close(ctx)

	var results []struct {
		SigHash []byte `bson:"sigHash"`
	}
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode sig hashes: %w", err)
	}

	hashes := make([]string, len(results))
	for i, r := range results {
		hashes[i] = hex.EncodeToString(r.SigHash)
	}
	return hashes, nil
}

// SignatureUpdate carries a signature and whether it was submitted via the backup (HTTP) path.
type SignatureUpdate struct {
	Bytes    []byte
	IsBackup bool
}

// UpdateSignatures updates transactions with new signatures
// Returns transactions that are now fully signed
func (s *StateStore) UpdateSignatures(
	ctx context.Context,
	signatures map[string]SignatureUpdate,
) ([]*Transaction, error) {
	fullySignedTxs := make([]*Transaction, 0)

	for sigHash, update := range signatures {
		sigBytes := update.Bytes
		sigHashBytes, decErr := hex.DecodeString(sigHash)
		if decErr != nil {
			return nil, fmt.Errorf("invalid sigHash %s: %w", sigHash, decErr)
		}
		filter := bson.M{"state": TxStatePending, "signatures.sigHash": sigHashBytes}

		var tx Transaction
		err := s.txCollection.FindOne(ctx, filter).Decode(&tx)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				continue
			}
			return nil, fmt.Errorf("failed to find transaction for sigHash %s: %w", sigHash, err)
		}

		for i := range tx.Signatures {
			if hex.EncodeToString(tx.Signatures[i].SigHash) == sigHash && tx.Signatures[i].Signature == nil {
				dbUpdate := bson.M{
					"$set": bson.M{
						fmt.Sprintf("signatures.%d.signature", i): sigBytes,
						fmt.Sprintf("signatures.%d.isBackup", i):  update.IsBackup,
					},
					"$inc": bson.M{
						"currentSignatures": 1,
					},
				}

				_, err := s.txCollection.UpdateOne(ctx, bson.M{"_id": tx.TxID}, dbUpdate)
				if err != nil {
					return nil, fmt.Errorf("failed to update signature for txID %s: %w", tx.TxID, err)
				}

				tx.Signatures[i].Signature = sigBytes
				tx.Signatures[i].IsBackup = update.IsBackup
				tx.CurrentSignatures++
				if tx.CurrentSignatures >= tx.TotalSignatures {
					fullySignedTxs = append(fullySignedTxs, &tx)
				}
				break
			}
		}
	}

	return fullySignedTxs, nil
}

// MarkTransactionSent atomically transitions a transaction from pending to sent,
// retaining signature data
func (s *StateStore) MarkTransactionSent(ctx context.Context, txID string) error {
	now := time.Now().UTC()
	result, err := s.txCollection.UpdateOne(
		ctx,
		bson.M{"_id": txID, "state": TxStatePending},
		bson.M{"$set": bson.M{"state": TxStateSent, "sentAt": now}},
	)
	if err != nil {
		return fmt.Errorf("failed to mark transaction as sent [txID:%s]: %w", txID, err)
	}
	if result.MatchedCount == 0 {
		return ErrTxNotFound
	}
	return nil
}

// MarkTransactionConfirmed atomically transitions a transaction from sent to confirmed
// and drops the raw tx and signature data
func (s *StateStore) MarkTransactionConfirmed(ctx context.Context, txID string) error {
	now := time.Now().UTC()
	result, err := s.txCollection.UpdateOne(
		ctx,
		bson.M{"_id": txID, "state": TxStateSent},
		bson.M{
			"$set":   bson.M{"state": TxStateConfirmed, "confirmedAt": now},
			"$unset": bson.M{"rawTx": "", "signatures": ""},
		},
	)
	if err != nil {
		return fmt.Errorf("failed to mark transaction as confirmed [txID:%s]: %w", txID, err)
	}
	if result.MatchedCount == 0 {
		return ErrTxNotFound
	}
	return nil
}

// IsTransactionProcessed checks if a transaction has been sent or confirmed
func (s *StateStore) IsTransactionProcessed(ctx context.Context, txID string) (bool, error) {
	count, err := s.txCollection.CountDocuments(ctx, bson.M{
		"_id":   txID,
		"state": bson.M{"$in": bson.A{TxStateSent, TxStateConfirmed}},
	})
	if err != nil {
		return false, fmt.Errorf("failed to check transaction [txID:%s]: %w", txID, err)
	}
	return count > 0, nil
}

// GetAllTransactions retrieves all sent and confirmed transaction IDs
func (s *StateStore) GetAllTransactions(ctx context.Context) ([]string, error) {
	cursor, err := s.txCollection.Find(
		ctx,
		bson.M{"state": bson.M{"$in": bson.A{TxStateSent, TxStateConfirmed}}},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get transactions: %w", err)
	}
	defer cursor.Close(ctx)

	var results []Transaction
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode results: %w", err)
	}

	txIDs := make([]string, len(results))
	for i, tx := range results {
		txIDs[i] = tx.TxID
	}
	return txIDs, nil
}

// GetSentTransactions returns all transactions in the "sent" state, including raw tx data.
func (s *StateStore) GetSentTransactions(ctx context.Context) ([]Transaction, error) {
	cursor, err := s.txCollection.Find(ctx, bson.M{"state": TxStateSent})
	if err != nil {
		return nil, fmt.Errorf("failed to get sent transactions: %w", err)
	}
	defer cursor.Close(ctx)

	var results []Transaction
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode sent transactions: %w", err)
	}
	return results, nil
}

// GetSentTransactionIDs returns the IDs of all transactions in the "sent" state
func (s *StateStore) GetSentTransactionIDs(ctx context.Context) ([]string, error) {
	cursor, err := s.txCollection.Find(ctx, bson.M{"state": TxStateSent})
	if err != nil {
		return nil, fmt.Errorf("failed to get sent transactions: %w", err)
	}
	defer cursor.Close(ctx)

	var results []Transaction
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("failed to decode results: %w", err)
	}

	txIDs := make([]string, len(results))
	for i, tx := range results {
		txIDs[i] = tx.TxID
	}
	return txIDs, nil
}

// DeleteOldPendingTransactions removes pending transactions created before the given age
func (s *StateStore) DeleteOldPendingTransactions(ctx context.Context, age time.Duration) (int64, error) {
	cutoffTime := time.Now().UTC().Add(-age)
	result, err := s.txCollection.DeleteMany(ctx, bson.M{
		"state":     TxStatePending,
		"createdAt": bson.M{"$lt": cutoffTime},
	})
	if err != nil {
		return 0, fmt.Errorf("failed to delete old pending transactions: %w", err)
	}
	return result.DeletedCount, nil
}

// DeleteOldSentTransactions removes sent transactions sent before the given age
func (s *StateStore) DeleteOldSentTransactions(ctx context.Context, age time.Duration) (int64, error) {
	cutoffTime := time.Now().UTC().Add(-age)
	result, err := s.txCollection.DeleteMany(ctx, bson.M{
		"state":  TxStateSent,
		"sentAt": bson.M{"$lt": cutoffTime},
	})
	if err != nil {
		return 0, fmt.Errorf("failed to delete old sent transactions: %w", err)
	}
	return result.DeletedCount, nil
}

// ClearAllTransactions removes all transaction records
func (s *StateStore) ClearAllTransactions(ctx context.Context) error {
	_, err := s.txCollection.DeleteMany(ctx, bson.M{})
	return err
}
