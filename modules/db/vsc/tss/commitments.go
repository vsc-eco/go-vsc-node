package tss_db

import (
	"context"
	"fmt"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type tssCommitments struct {
	*db.Collection
}

func (tsc *tssCommitments) SetCommitmentData(commitment TssCommitment) error {
	options := options.FindOneAndUpdate().SetUpsert(true)
	updateResult := tsc.FindOneAndUpdate(context.Background(), bson.M{
		"key_id": commitment.KeyId,
		"tx_id":  commitment.TxId,
	}, bson.M{
		"$set": bson.M{
			"type":         commitment.Type,
			"block_height": commitment.BlockHeight,
			"epoch":        commitment.Epoch,
			"key_id":       commitment.KeyId,
			"commitment":   commitment.Commitment,
			"public_key":   commitment.PublicKey,
			"tx_id":        commitment.TxId,
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

	return commitments, nil
}

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
	return commitments, nil
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

	return commitments, nil
}

func NewCommitments(d *vsc.VscDb) TssCommitments {
	return &tssCommitments{db.NewCollection(d.DbInstance, "tss_commitments")}
}

// review2 HIGH #27: tss_commitments is queried by {key_id, block_height}
// (GetCommitmentByHeight) with only the _id index.
func (e *tssCommitments) Init() error {
	if err := e.Collection.Init(); err != nil {
		return err
	}
	return e.CreateIndexIfNotExist(mongo.IndexModel{
		Keys: bson.D{{Key: "key_id", Value: 1}, {Key: "block_height", Value: -1}},
	})
}
