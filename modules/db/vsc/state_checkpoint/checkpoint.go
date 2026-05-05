// Package state_checkpoint stores the slot height of the most recent fully
// committed state-engine slot transaction. It is written as the very last
// upsert inside each slot's MongoDB transaction so that crash recovery can
// distinguish a fully applied slot from one whose transaction was aborted.
package state_checkpoint

import (
	"context"
	"errors"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// docId is the stable _id of the singleton checkpoint document.
const docId = "current"

type Checkpoint struct {
	Id               string `bson:"_id"`
	LastCommittedSlot uint64 `bson:"last_committed_slot"`
	// LastCommittedAtUnix is set with the wall-clock time the txn committed.
	// It is informational only; recovery depends on LastCommittedSlot.
	LastCommittedAtUnix int64 `bson:"last_committed_at_unix"`
}

type StateCheckpoint interface {
	aggregate.Plugin
	// Set writes the checkpoint inside the supplied (session) context.
	// Callers using a Mongo session should pass the session context so the
	// write joins the slot transaction.
	Set(ctx context.Context, slotHeight uint64, nowUnix int64) error
	// Get returns the most recent committed slot, or 0 if no checkpoint
	// has been written yet (genesis startup).
	Get(ctx context.Context) (uint64, error)
}

type checkpoint struct {
	*db.Collection
}

var _ StateCheckpoint = (*checkpoint)(nil)

func New(d *vsc.VscDb) StateCheckpoint {
	return &checkpoint{db.NewCollection(d.DbInstance, "state_checkpoint")}
}

func (c *checkpoint) Set(ctx context.Context, slotHeight uint64, nowUnix int64) error {
	opts := options.Update().SetUpsert(true)
	_, err := c.UpdateOne(ctx, bson.M{"_id": docId}, bson.M{
		"$set": bson.M{
			"last_committed_slot":    slotHeight,
			"last_committed_at_unix": nowUnix,
		},
	}, opts)
	return err
}

func (c *checkpoint) Get(ctx context.Context) (uint64, error) {
	var doc Checkpoint
	err := c.FindOne(ctx, bson.M{"_id": docId}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil
		}
		return 0, err
	}
	return doc.LastCommittedSlot, nil
}
