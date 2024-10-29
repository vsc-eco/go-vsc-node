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

func New(d *vsc.VscDb) HiveBlocks {
	return &hiveBlocks{db.NewCollection(d.DbInstance, "hive_blocks")}
}

// stores a block alongside updating our last processed block
func (h *hiveBlocks) StoreBlock(block *HiveBlock) error {
	if block == nil {
		return fmt.Errorf("block is nil")
	}

	_, err := h.InsertOne(context.Background(), bson.M{
		"type":  "block",
		"block": block,
	})
	if err != nil {
		return fmt.Errorf("failed to store block: %v", err)
	}

	// update the last processed block number
	err = h.StoreLastProcessedBlock(block.BlockNumber)
	if err != nil {
		// ignore this case, we'll simply overwrite later, worst case it makes
		// our next call a little less efficient
		//
		// leaving this err != nil check here for clarity
	}
	return nil
}

// deletes all block and metadata documents from the coll
func (h *hiveBlocks) ClearBlocks() error {
	// delete blocks
	_, err := h.DeleteMany(context.Background(), bson.M{"type": "block"})
	if err != nil {
		return fmt.Errorf("failed to clear blocks collection: %v", err)
	}

	// delete metadata
	_, err = h.DeleteMany(context.Background(), bson.M{"type": "metadata"})
	if err != nil {
		return fmt.Errorf("failed to clear metadata: %v", err)
	}

	return nil
}

// upserts the last processed block number in the coll
func (h *hiveBlocks) StoreLastProcessedBlock(blockNumber int) error {
	_, err := h.UpdateOne(context.Background(), bson.M{"type": "metadata"},
		bson.M{"$set": bson.M{"block_number": blockNumber}},
		options.Update().SetUpsert(true),
	)
	return err
}

// retrieves the last processed block number from the coll
func (h *hiveBlocks) GetLastProcessedBlock() (int, error) {
	var result struct {
		BlockNumber int `bson:"block_number"`
	}
	err := h.FindOne(context.Background(), bson.M{"type": "metadata"}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get last processed block: %v", err)
	}
	return result.BlockNumber, nil
}

// retrieves blocks within a specified range, ordered by block number in asc order
func (h *hiveBlocks) FetchStoredBlocks(startBlock, endBlock int) ([]HiveBlock, error) {
	filter := bson.M{
		"type": "block",
		"block.block_number": bson.M{
			"$gte": startBlock,
			"$lte": endBlock,
		},
	}

	// sort blocks by block number in ascending order
	findOptions := options.Find().SetSort(bson.D{{Key: "block.block_number", Value: 1}})
	cursor, err := h.Find(context.Background(), filter, findOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blocks: %v", err)
	}
	defer cursor.Close(context.Background())

	var wrappedBlocks []struct {
		Block HiveBlock `bson:"block"`
	}

	if err := cursor.All(context.Background(), &wrappedBlocks); err != nil {
		return nil, fmt.Errorf("failed to decode blocks: %v", err)
	}

	// flatten out the blocks into a plain slice
	blocks := make([]HiveBlock, len(wrappedBlocks))
	for i, wrappedBlock := range wrappedBlocks {
		blocks[i] = wrappedBlock.Block
	}

	return blocks, nil
}