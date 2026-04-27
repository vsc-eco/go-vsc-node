package rcDb

import (
	"context"
	"fmt"
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

	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "account", Value: 1}, {Key: "block_height", Value: -1}}},
	}
	for _, idx := range indexes {
		if err := e.CreateIndexIfNotExist(idx); err != nil {
			return fmt.Errorf("failed to create rcs index: %w", err)
		}
	}
	return nil
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

// GetRecordsBulk returns the most recent RC record per account at or before blockHeight.
// Accounts with no record are absent from the returned map.
func (e *rcDb) GetRecordsBulk(ctx context.Context, accounts []string, blockHeight uint64) (map[string]RcRecord, error) {
	out := make(map[string]RcRecord, len(accounts))
	if len(accounts) == 0 {
		return out, nil
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"account":      bson.M{"$in": accounts},
			"block_height": bson.M{"$lte": blockHeight},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "account", Value: 1}, {Key: "block_height", Value: -1}}}},
		{{Key: "$group", Value: bson.M{
			"_id": "$account",
			"doc": bson.M{"$first": "$$ROOT"},
		}}},
		{{Key: "$replaceRoot", Value: bson.M{"newRoot": "$doc"}}},
	}
	cursor, err := e.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var rec RcRecord
		if err := cursor.Decode(&rec); err != nil {
			return nil, err
		}
		out[rec.Account] = rec
	}
	return out, nil
}

// SetRecordsBulk upserts a batch of RC records in a single round-trip. Order is unimportant
// because each upsert keys on (account, block_height) which is unique within the batch.
func (e *rcDb) SetRecordsBulk(ctx context.Context, records []RcRecord) error {
	if len(records) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(records))
	for _, rec := range records {
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"account": rec.Account, "block_height": rec.BlockHeight}).
			SetUpdate(bson.M{"$set": bson.M{"amount": rec.Amount}}).
			SetUpsert(true))
	}
	_, err := e.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	return err
}

// PruneOlderThan deletes all rcs records with block_height < cutoff. Safe because
// CalculateFrozenBal returns 0 once (current - record.BlockHeight) >= RC_RETURN_PERIOD
// (rc-system/utils.go:7-12), so any record older than that contributes 0 to frozen
// regardless of whether it's present. UpdateRcMap handles "no record" via its
// rcRecord.BlockHeight == 0 fallback (state_engine.go: same rcBal = v result).
// Caller must pass cutoff <= currentBlock - RC_RETURN_PERIOD with a comfortable
// safety margin.
func (e *rcDb) PruneOlderThan(ctx context.Context, cutoff uint64) (int64, error) {
	if cutoff == 0 {
		return 0, nil
	}
	res, err := e.DeleteMany(ctx, bson.M{"block_height": bson.M{"$lt": cutoff}})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}
