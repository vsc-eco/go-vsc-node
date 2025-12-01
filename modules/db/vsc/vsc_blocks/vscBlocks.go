package vscBlocks

import (
	"context"
	"fmt"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type vscBlocks struct {
	*db.Collection
}

func New(d *vsc.VscDb) VscBlocks {
	return &vscBlocks{db.NewCollection(d.DbInstance, "block_headers")}
}

func (blocks *vscBlocks) Init() error {
	err := blocks.Collection.Init()
	if err != nil {
		return err
	}

	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "slot_height", Value: 1}},
		Options: options.Index().SetUnique(true),
	}

	// create index on block.block_number for faster queries
	err = blocks.CreateIndexIfNotExist(indexModel)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}

	indexModel = mongo.IndexModel{
		Keys:    bson.D{{Key: "id", Value: 1}},
		Options: options.Index().SetUnique(true),
	}

	// create index on block.block_number for faster queries
	err = blocks.CreateIndexIfNotExist(indexModel)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}

	return nil
}

func (vblks *vscBlocks) StoreHeader(header VscHeaderRecord) {
	opts := options.FindOneAndUpdate()
	opts.SetUpsert(true)
	vblks.FindOneAndUpdate(context.Background(), bson.M{"id": header.Id}, bson.M{"$set": header}, opts)
}

// Gets VSC block by height
func (vblks *vscBlocks) GetBlockByHeight(height uint64) (*VscHeaderRecord, error) {
	ctx := context.Background()
	findOptions := options.FindOne().SetSort(bson.M{
		"slot_height": -1,
	})

	slotFilter := bson.M{
		"slot_height": bson.M{
			"$lte": height,
		},
	}
	findResult := vblks.FindOne(ctx, slotFilter, findOptions)

	if findResult.Err() != nil {
		return nil, findResult.Err()
	}

	var header VscHeaderRecord

	err := findResult.Decode(&header)
	if err != nil {
		return nil, err
	}

	return &header, nil
}

func (vblks *vscBlocks) GetBlockById(id string) (*VscHeaderRecord, error) {
	ctx := context.Background()

	findResult := vblks.FindOne(ctx, bson.M{
		"id": bson.M{
			"$eq": id,
		},
	})

	if findResult.Err() != nil {
		return nil, findResult.Err()
	}

	var header VscHeaderRecord
	err := findResult.Decode(&header)

	if err != nil {
		return nil, err
	}

	return &header, nil
}

func (vblks *vscBlocks) GetBlocksByElection(epoch uint64) ([]VscHeaderRecord, error) {
	// Get all VSC blocks for a given election epoch
	ctx := context.Background()

	cursor, err := vblks.Find(ctx, bson.M{
		"epoch": bson.M{
			"$eq": epoch,
		},
	})

	if err != nil {
		return nil, err
	}

	var headers []VscHeaderRecord
	err = cursor.All(ctx, &headers)
	if err != nil {
		return nil, err
	}

	return headers, nil
}
