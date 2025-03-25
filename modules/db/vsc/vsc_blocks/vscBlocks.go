package vscBlocks

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type vscBlocks struct {
	*db.Collection
}

func New(d *vsc.VscDb) VscBlocks {
	return &vscBlocks{db.NewCollection(d.DbInstance, "block_headers")}
}

func (vblks *vscBlocks) StoreHeader(header VscHeaderRecord) {
	opts := options.FindOneAndUpdate()
	opts.SetUpsert(true)
	vblks.FindOneAndUpdate(context.Background(), bson.M{"id": header.Id}, bson.M{"$set": header}, opts)
}

// Gets VSC block by height
func (vblks *vscBlocks) GetBlockByHeight(height uint64) (*VscHeaderRecord, error) {
	ctx := context.Background()

	slotFilter := bson.M{
		"slot_height": bson.M{
			"$lte": height,
		},
	}
	findResult := vblks.FindOne(ctx, slotFilter)

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
