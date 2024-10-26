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

// hiveBlocks is the unexported implementation of the HiveBlocks interface.
type hiveBlocks struct {
	*db.Collection
}

// inits and returns a HiveBlocks interface with the underlying hiveBlocks struct.
func New(vscDb *vsc.VscDb) HiveBlocks {
	return &hiveBlocks{db.NewCollection(vscDb.DbInstance, "hive_blocks")}
}

func (h *hiveBlocks) StoreBlock(block *HiveBlock) error {
	if block == nil {
		return fmt.Errorf("block is nil")
	}

	_, err := h.InsertOne(context.TODO(), bson.M{
		"type":  "block",
		"block": block,
	})
	if err != nil {
		return fmt.Errorf("failed to store block: %v", err)
	}

	// update the last processed block number
	err = h.StoreLastProcessedBlock(block.BlockNumber)
	if err != nil {
		fmt.Printf("Warning: failed to update last processed block number: %v\n", err)
	}
	return nil
}

// deletes all block and metadata documents from the collection.
func (h *hiveBlocks) ClearBlocks() error {
	// start a session for tx
	session, err := h.Database().Client().StartSession()
	if err != nil {
		return fmt.Errorf("failed to start session: %v", err)
	}
	defer session.EndSession(context.Background())

	// define a tx func
	callback := func(sessCtx mongo.SessionContext) (interface{}, error) {
		// delete blocks
		_, err := h.DeleteMany(sessCtx, bson.M{"type": "block"})
		if err != nil {
			return nil, fmt.Errorf("failed to clear blocks collection: %v", err)
		}

		// delete metadata
		_, err = h.DeleteMany(sessCtx, bson.M{"type": "metadata"})
		if err != nil {
			return nil, fmt.Errorf("failed to clear metadata: %v", err)
		}

		return nil, nil
	}

	// execute the tx
	_, err = session.WithTransaction(context.Background(), callback)
	if err != nil {
		return fmt.Errorf("transaction failed: %v", err)
	}

	return nil
}

// upserts the last processed block number in the collection.
func (h *hiveBlocks) StoreLastProcessedBlock(blockNumber int) error {
	_, err := h.UpdateOne(context.Background(), bson.M{"type": "metadata"},
		bson.M{"$set": bson.M{"block_number": blockNumber}},
		options.Update().SetUpsert(true),
	)
	return err
}

// retrieves the last processed block number from the collection.
func (h *hiveBlocks) GetLastProcessedBlock() (int, error) {
	var result struct {
		BlockNumber int `bson:"block_number"`
	}
	err := h.FindOne(context.Background(), bson.M{"type": "metadata"}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			fmt.Println("Warning: no last processed block found")
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get last processed block: %v", err)
	}
	return result.BlockNumber, nil
}

// retrieves blocks within a specified range.
func (h *hiveBlocks) FetchStoredBlocks(startBlock, endBlock int) ([]HiveBlock, error) {
	filter := bson.M{
		"type": "block",
		"block.block_number": bson.M{
			"$gte": startBlock,
			"$lte": endBlock,
		},
	}
	cursor, err := h.Find(context.Background(), filter)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blocks: %v", err)
	}
	defer cursor.Close(context.Background())

	var blocks []HiveBlock
	if err := cursor.All(context.Background(), &blocks); err != nil {
		return nil, fmt.Errorf("failed to decode blocks: %v", err)
	}
	return blocks, nil
}
