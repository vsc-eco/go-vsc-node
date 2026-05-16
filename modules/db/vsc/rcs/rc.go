package rcDb

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type rcDb struct {
	*db.Collection
}

func New(d *vsc.VscDb) RcDb {
	return &rcDb{db.NewCollection(d.DbInstance, "rcs")}
}

func (e *rcDb) Init() error {
	err := e.Collection.Init()
	if err != nil {
		return err
	}

	// review2 HIGH #27: rcs is read by {account, latest block_height}
	// (GetRecord) with only the _id index.
	return e.CreateIndexIfNotExist(mongo.IndexModel{
		Keys: bson.D{{Key: "account", Value: 1}, {Key: "block_height", Value: -1}},
	})
}

func (e *rcDb) GetRecord(account string, blockHeight uint64) (RcRecord, error) {

	query := bson.M{
		"account":      account,
		"block_height": bson.M{"$lte": blockHeight},
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "block_height", Value: -1}})

	findResult := e.Collection.FindOne(context.Background(), query, opts)

	var record RcRecord
	err := findResult.Decode(&record)

	if err != nil {
		return record, err
	}

	return record, nil
}

func (e rcDb) SetRecord(account string, blockHeight uint64, amount int64) {
	query := bson.M{
		"account":      account,
		"block_height": blockHeight,
	}
	options := options.FindOneAndUpdate().SetUpsert(true)
	e.Collection.FindOneAndUpdate(context.Background(), query, bson.M{
		"$set": bson.M{
			"amount": amount,
		},
	}, options)
}
