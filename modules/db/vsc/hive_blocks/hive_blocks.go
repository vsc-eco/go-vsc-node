package hive_blocks

import (
	"context"
	"fmt"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type hiveBlocks struct {
	*db.Collection
}

// creates a new collection in the database
func New(d *vsc.VscDb) HiveBlocks {
	return &hiveBlocks{
		Collection: db.NewCollection(d.DbInstance, "hive_blocks"),
	}
}

// stores a block in the db
func (h *hiveBlocks) StoreBlock(block *HiveBlock) error {
	if block == nil {
		return fmt.Errorf("block is nil")
	}

	// insert block into the collection
	_, err := h.Collection.Collection.InsertOne(context.Background(), block)
	if err != nil {
		return fmt.Errorf("failed to store block: %v", err)
	}

	return nil
}

// clears all blocks from this collection
func (h *hiveBlocks) ClearBlocks() error {
	_, err := h.Collection.Collection.DeleteMany(context.Background(), bson.M{})
	return err
}

// stores the last processed block number in the database
func (h *hiveBlocks) StoreLastProcessedBlock(blockNumber int) error {
	metaCollection := h.Collection.Collection.Database().Collection("hive_blocks_meta")

	_, err := metaCollection.UpdateOne(context.Background(), bson.M{
		"type": "last_processed_block",
	}, bson.M{
		"$set": bson.M{
			"block_number": blockNumber,
		},
		// upserts so that if no record exists, create it
	}, options.Update().SetUpsert(true))
	return err
}

// retrieves the last processed block number from the db
func (h *hiveBlocks) GetLastProcessedBlock() (int, error) {
	metaCollection := h.Collection.Collection.Database().Collection("hive_blocks_meta")

	var result struct {
		BlockNumber int `bson:"block_number"`
	}

	err := metaCollection.FindOne(context.Background(), bson.M{
		"type": "last_processed_block",
	}).Decode(&result)

	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil // if no doc is found, return 0 as the last processed block
		}
		return 0, fmt.Errorf("failed to get last processed block: %v", err)
	}

	return result.BlockNumber, nil
}

// fetches blocks within a specified range
func (h *hiveBlocks) FetchStoredBlocks(startBlock int, endBlock int) ([]HiveBlock, error) {
	filter := bson.M{
		// creating a range, aka: startBlock <= block_number <= endBlock
		"block_number": bson.M{
			"$gte": startBlock,
			"$lte": endBlock,
		},
	}

	// fetch the blocks from the collection
	cursor, err := h.Collection.Collection.Find(context.Background(), filter)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blocks: %v", err)
	}
	defer cursor.Close(context.Background())

	// decode the blocks into a slice
	var blocks []HiveBlock
	if err := cursor.All(context.Background(), &blocks); err != nil {
		return nil, fmt.Errorf("failed to decode blocks: %v", err)
	}

	return blocks, nil
}
