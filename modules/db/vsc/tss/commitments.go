package tss_db

import (
	"context"
	"fmt"
	"sort"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// DedupCommitmentsBySemanticKey collapses rows sharing
// (key_id, block_height, type) to a single representative — the row with the
// lexicographically smallest tx_id, picked deterministically so every node
// computes the same result.
//
// Why this exists: SetCommitmentData used to dedup on (key_id, tx_id) and
// retries with new tx_ids created multiple rows per semantic commitment
// (audit S8). The current SetCommitmentData filter is now
// (key_id, block_height, type), so new inserts are dedup'd at write time —
// but rows written before the fix may still exist in production DBs.
// Without read-time dedup, consumers that iterate every row (tss.go blame
// aggregation, state_engine.go pendulum reductions) would count those
// historical duplicates multiple times, inflating blame scores or reductions.
// Apply this helper at every read boundary that aggregates over rows.
//
// Exported so mock_tss.go in lib/test_utils can apply the same semantics
// to mock-backed tests — otherwise mock and prod would silently diverge.
func DedupCommitmentsBySemanticKey(commits []TssCommitment) []TssCommitment {
	if len(commits) <= 1 {
		return commits
	}
	type semanticKey struct {
		KeyId       string
		BlockHeight uint64
		Type        string
	}
	winner := make(map[semanticKey]TssCommitment, len(commits))
	for _, c := range commits {
		k := semanticKey{KeyId: c.KeyId, BlockHeight: c.BlockHeight, Type: c.Type}
		prev, exists := winner[k]
		if !exists || c.TxId < prev.TxId {
			winner[k] = c
		}
	}
	out := make([]TssCommitment, 0, len(winner))
	for _, c := range winner {
		out = append(out, c)
	}
	// Deterministic order so downstream iteration is stable across nodes.
	sort.Slice(out, func(i, j int) bool {
		if out[i].BlockHeight != out[j].BlockHeight {
			return out[i].BlockHeight < out[j].BlockHeight
		}
		if out[i].KeyId != out[j].KeyId {
			return out[i].KeyId < out[j].KeyId
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].TxId < out[j].TxId
	})
	return out
}

type tssCommitments struct {
	*db.Collection
}

func (tsc *tssCommitments) SetCommitmentData(commitment TssCommitment) error {
	// S8: dedup on (key_id, block_height, type), not (key_id, tx_id). A
	// retried commitment in a fresh custom_json keeps its semantic identity
	// (same key, same block, same type) but gets a new tx_id, which used
	// to insert a second row. Two rows broke GetCommitmentByHeight's
	// "highest row wins" assumption and let any witness rewind keyInfo.Epoch
	// by re-broadcasting an older commitment for the same active key.
	//
	// Defense in depth: log when an existing row's commitment bytes differ
	// from the incoming payload. Under honest 2/3 BLS aggregation only one
	// valid commitment can exist for (key_id, block_height, type), so a
	// non-equal overwrite is either a soft-fork window or evidence of a
	// double-aggregate attempt — either way operators want to see it.
	//
	// This FindOne→FindOneAndUpdate pair is NOT transactional, which is
	// safe today because SetCommitmentData is invoked serially from
	// state-engine.ProcessBlock (state_engine.go:1095). If a future caller
	// ever runs this concurrently, two racing inserts could both miss the
	// warning, so keep the single-writer invariant or move to a Mongo
	// transaction first.
	existing := tsc.FindOne(context.Background(), bson.M{
		"key_id":       commitment.KeyId,
		"block_height": commitment.BlockHeight,
		"type":         commitment.Type,
	})
	if existing.Err() == nil {
		var prev TssCommitment
		if decodeErr := existing.Decode(&prev); decodeErr == nil {
			if prev.Commitment != commitment.Commitment || prev.Epoch != commitment.Epoch {
				log.Warn("SetCommitmentData: overwriting non-equal commitment row",
					"keyId", commitment.KeyId, "type", commitment.Type, "blockHeight", commitment.BlockHeight,
					"prevEpoch", prev.Epoch, "newEpoch", commitment.Epoch,
					"prevTxId", prev.TxId, "newTxId", commitment.TxId)
			}
		}
	}

	options := options.FindOneAndUpdate().SetUpsert(true)
	updateResult := tsc.FindOneAndUpdate(context.Background(), bson.M{
		"key_id":       commitment.KeyId,
		"block_height": commitment.BlockHeight,
		"type":         commitment.Type,
	}, bson.M{
		"$set": bson.M{
			"type":         commitment.Type,
			"block_height": commitment.BlockHeight,
			"epoch":        commitment.Epoch,
			"key_id":       commitment.KeyId,
			"commitment":   commitment.Commitment,
			"public_key":   commitment.PublicKey,
			"tx_id":        commitment.TxId,
			"bv":           commitment.BitSet,
		},
	}, options)

	dbErr := updateResult.Err()
	// FindOneAndUpdate with upsert returns ErrNoDocuments when it inserts a new
	// document (nothing to "find"), but the write still succeeded.
	if dbErr != nil && dbErr != mongo.ErrNoDocuments {
		log.Warn("SetCommitmentData failed", "keyId", commitment.KeyId, "type", commitment.Type, "epoch", commitment.Epoch, "txId", commitment.TxId, "err", dbErr)
		return dbErr
	}
	log.Verbose("SetCommitmentData OK", "keyId", commitment.KeyId, "type", commitment.Type, "epoch", commitment.Epoch, "blockHeight", commitment.BlockHeight, "txId", commitment.TxId, "db", tsc.Database().Name())
	return nil
}

func (tsc *tssCommitments) GetCommitment(keyId string, epoch uint64) (TssCommitment, error) {
	findResult := tsc.FindOne(context.Background(), bson.M{
		"key_id": keyId,
		"epoch":  epoch,
	})

	if findResult.Err() != nil {
		return TssCommitment{}, findResult.Err()
	}
	var record TssCommitment
	err := findResult.Decode(&record)

	if err != nil {
		return TssCommitment{}, err
	}

	return record, nil
}

func (tsc *tssCommitments) GetCommitmentByHeight(keyId string, height uint64, qtype ...string) (TssCommitment, error) {
	findOpts := options.FindOne().SetSort(bson.M{
		"block_height": -1,
	})

	query := bson.M{
		"key_id": keyId,
		"block_height": bson.M{
			"$lt": height,
		},
	}

	if len(qtype) > 0 {
		query["type"] = bson.M{
			"$in": qtype,
		}
	}

	log.Trace("getCommitmentByHeight", "query", query)

	findResult := tsc.FindOne(context.Background(), query, findOpts)

	if findResult.Err() != nil {
		return TssCommitment{}, findResult.Err()
	}
	var commitment TssCommitment
	err := findResult.Decode(&commitment)

	return commitment, err
}

// FindCommitments returns commitment rows matching the filter, with
// (offset, limit) applied by the mongo aggregation BEFORE the S8
// dedup helper runs. That means a paginated response can shrink below
// the requested limit if a page lands on a cluster of pre-fix duplicate
// rows. Consensus-critical callers pass (0, 0) (no limit) so they are
// unaffected; the GQL explorer caller (schema.resolvers.go) accepts the
// short-page behavior. If a future caller needs strict-N pagination,
// either over-fetch (limit ×2) or push the dedup into the aggregation
// pipeline via a $group stage.
func (tsc *tssCommitments) FindCommitments(keyId *string, byTypes []string, epoch *uint64, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]TssCommitment, error) {
	filters := bson.D{}
	if keyId != nil {
		filters = append(filters, bson.E{Key: "key_id", Value: *keyId})
	}
	if len(byTypes) > 0 {
		filters = append(filters, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: byTypes}}})
	}
	if epoch != nil {
		filters = append(filters, bson.E{Key: "epoch", Value: *epoch})
	}
	if fromBlock != nil {
		filters = append(filters, bson.E{Key: "block_height", Value: bson.D{{Key: "$gt", Value: *fromBlock}}})
	}
	if toBlock != nil {
		filters = append(filters, bson.E{Key: "block_height", Value: bson.D{{Key: "$lte", Value: *toBlock}}})
	}

	pipe := hive_blocks.GetAggTimestampPipeline(filters, "block_height", "timestamp", offset, limit)
	cursor, err := tsc.Aggregate(context.Background(), pipe)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	commitments := make([]TssCommitment, 0)
	for cursor.Next(context.Background()) {
		var commitment TssCommitment
		if err := cursor.Decode(&commitment); err != nil {
			return nil, fmt.Errorf("failed to decode commitment: %w", err)
		}
		commitments = append(commitments, commitment)
	}

	return DedupCommitmentsBySemanticKey(commitments), nil
}

// FindCommitmentsSimple has the same dedup-after-limit caveat as
// FindCommitments. The TSS blame-window callers in modules/tss use
// limit=100, large enough that pre-migration duplicate clustering at
// the head of the sort is unlikely to materially under-count blames.
func (tsc *tssCommitments) FindCommitmentsSimple(keyId *string, byTypes []string, epoch *uint64, fromBlock *uint64, toBlock *uint64, limit int) ([]TssCommitment, error) {
	query := bson.M{}
	if keyId != nil {
		query["key_id"] = *keyId
	}
	if len(byTypes) > 0 {
		query["type"] = bson.M{"$in": byTypes}
	}
	if epoch != nil {
		query["epoch"] = *epoch
	}
	if fromBlock != nil {
		query["block_height"] = bson.M{"$gt": *fromBlock}
	}
	if toBlock != nil {
		if _, exists := query["block_height"]; exists {
			query["block_height"].(bson.M)["$lte"] = *toBlock
		} else {
			query["block_height"] = bson.M{"$lte": *toBlock}
		}
	}

	findOpts := options.Find().SetSort(bson.M{"block_height": -1})
	if limit > 0 {
		findOpts.SetLimit(int64(limit))
	}

	cursor, err := tsc.Find(context.Background(), query, findOpts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	commitments := make([]TssCommitment, 0)
	for cursor.Next(context.Background()) {
		var commitment TssCommitment
		if err := cursor.Decode(&commitment); err != nil {
			return nil, fmt.Errorf("failed to decode commitment: %w", err)
		}
		commitments = append(commitments, commitment)
	}
	return DedupCommitmentsBySemanticKey(commitments), nil
}

func (tsc *tssCommitments) GetBlames(epoch *uint64) ([]TssCommitment, error) {
	query := bson.M{
		"type": "blame",
	}
	if epoch != nil {
		query["epoch"] = *epoch
	}

	findResult, err := tsc.Find(context.Background(), query)
	if err != nil {
		return nil, err
	}
	defer findResult.Close(context.Background())

	commitments := make([]TssCommitment, 0)
	for findResult.Next(context.Background()) {
		var commitment TssCommitment
		if err := findResult.Decode(&commitment); err != nil {
			return nil, fmt.Errorf("failed to decode commitment: %w", err)
		}
		commitments = append(commitments, commitment)
	}

	return DedupCommitmentsBySemanticKey(commitments), nil
}

func NewCommitments(d *vsc.VscDb) TssCommitments {
	return &tssCommitments{db.NewCollection(d.DbInstance, "tss_commitments")}
}

// review2 HIGH #27: tss_commitments is queried by {key_id, block_height}
// (GetCommitmentByHeight) with only the _id index.
//
// S8 follow-up: SetCommitmentData now upserts on (key_id, block_height, type),
// so the index is extended to include type. Mongo can still serve the
// (key_id, block_height) prefix lookups from this index.
func (e *tssCommitments) Init() error {
	if err := e.Collection.Init(); err != nil {
		return err
	}
	return e.CreateIndexIfNotExist(mongo.IndexModel{
		Keys: bson.D{
			{Key: "key_id", Value: 1},
			{Key: "block_height", Value: -1},
			{Key: "type", Value: 1},
		},
	})
}
