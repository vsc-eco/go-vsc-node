package tss_db

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type tssCommitments struct {
	*db.Collection
}

func (tsc *tssCommitments) SetCommitmentData(commitment TssCommitment) error {
	options := options.FindOneAndUpdate().SetUpsert(true)
	updateResult := tsc.FindOneAndUpdate(context.Background(), bson.M{
		"tx_id": commitment.TxId,
	}, bson.M{
		"type":         commitment.Type,
		"block_height": commitment.BlockHeight,
		"epoch":        commitment.Epoch,
		"key_id":       commitment.KeyId,
	}, options)

	return updateResult.Err()
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

func (tsc *tssCommitments) GetLatestCommitment(keyId string) (TssCommitment, error) {
	findOpts := options.FindOne().SetSort(bson.M{
		"epoch": -1,
	})

	findResult := tsc.FindOne(context.Background(), bson.M{
		"key_id": keyId,
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

func NewCommitments(d *vsc.VscDb) TssCommitments {
	return &tssCommitments{db.NewCollection(d.DbInstance, "tss_commitments")}
}
