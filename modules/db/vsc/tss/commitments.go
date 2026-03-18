package tss_db

import (
	"context"
	"fmt"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

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

func (tsc *tssCommitments) GetLatestCommitment(keyId string, qtype string) (TssCommitment, error) {
	findOpts := options.FindOne().SetSort(bson.M{
		"epoch": -1,
	})

	findResult := tsc.FindOne(context.Background(), bson.M{
		"key_id": keyId,
		"type":   qtype,
	}, findOpts)

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

func (tsc *tssCommitments) FindCommitments(keyId string, opts ...SearchOption) ([]TssCommitment, error) {
	query := bson.M{
		"key_id": keyId,
	}
	for _, opt := range opts {
		if err := opt(&query); err != nil {
			return nil, err
		}
	}

	findOpts := options.Find().
		SetSort(bson.D{{Key: "block_height", Value: -1}, {Key: "epoch", Value: -1}})

	cursor, err := tsc.Find(context.Background(), query, findOpts)
	if err != nil {
		return nil, err
	}

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

func (tsc *tssCommitments) FindAllCommitments(opts ...SearchOption) ([]TssCommitment, error) {
	query := bson.M{}
	for _, opt := range opts {
		if err := opt(&query); err != nil {
			return nil, err
		}
	}

	findOpts := options.Find().
		SetSort(bson.D{{Key: "block_height", Value: -1}, {Key: "epoch", Value: -1}})

	cursor, err := tsc.Find(context.Background(), query, findOpts)
	if err != nil {
		return nil, err
	}

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

func (tsc *tssCommitments) GetBlames(opts ...SearchOption) ([]TssCommitment, error) {
	query := bson.M{
		"type": "blame",
	}
	for _, opt := range opts {
		if err := opt(&query); err != nil {
			return nil, err
		}
	}

	findResult, err := tsc.Find(context.Background(), query)

	if err != nil {
		return nil, err
	}
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
