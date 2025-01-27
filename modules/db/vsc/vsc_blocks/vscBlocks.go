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

func (vblks *vscBlocks) StoreHeader(header VscHeader) {
	opts := options.FindOneAndUpdate()
	opts.SetUpsert(true)
	vblks.FindOneAndUpdate(context.Background(), bson.M{"id": header.Id}, bson.M{"$set": header}, opts)
}
