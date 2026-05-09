package db

import (
	"context"
	"slices"
	"vsc-node/lib/vsclog"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var log = vsclog.Module("db")

var REINDEX_ID = 18

var IMMUTABLE_COLLECTIONS = []string{
	"hive_blocks",
}

type DbReindex struct {
	*DbInstance
	force bool
}

func (dbr *DbReindex) Init() error {
	ctx := context.Background()
	col := dbr.Collection("hive_blocks")
	findResult := col.FindOne(ctx, bson.M{
		"type": "metadata",
	})
	result := SearchResult{}
	err := findResult.Decode(&result)

	var indexId uint64
	if err != nil || result.ReindexId == nil {
		indexId = 0
	} else {
		indexId = *result.ReindexId
		log.Verbose("reindex metadata", "reindex_id", indexId)
	}

	if indexId != uint64(REINDEX_ID) || dbr.force {
		if dbr.force {
			log.Info("force reindexing database", "from", indexId, "to", REINDEX_ID)
		} else {
			log.Info("reindexing database", "from", indexId, "to", REINDEX_ID)
		}
		cols, _ := dbr.ListCollectionNames(ctx, bson.M{})

		for _, name := range cols {
			if !slices.Contains(IMMUTABLE_COLLECTIONS, name) {
				col := dbr.Collection(name)
				col.DeleteMany(ctx, bson.M{})
			}
		}

		col.FindOneAndUpdate(ctx, bson.M{
			"type": "metadata",
		}, bson.M{
			"$set": bson.M{
				"reindex_id":           REINDEX_ID,
				"last_processed_block": 0,
			},
		}, options.FindOneAndUpdate().SetUpsert(true))
	}
	return nil
}

func NewReindex(db *DbInstance, force bool) *DbReindex {
	return &DbReindex{db, force}
}

type SearchResult struct {
	ReindexId *uint64 `json:"reindex_id,omitempty" bson:"reindex_id,omitempty"`
}
