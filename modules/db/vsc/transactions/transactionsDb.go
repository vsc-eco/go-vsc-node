package transactions

import (
	"context"
	"fmt"
	"time"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type transactions struct {
	*db.Collection
}

func New(d *vsc.VscDb) Transactions {
	return &transactions{db.NewCollection(d.DbInstance, "transaction_pool")}
}

func (e *transactions) Init() error {
	err := e.Collection.Init()
	if err != nil {
		return err
	}

	return nil
}

func (e *transactions) Ingest(offTx IngestTransactionUpdate) error {
	fmt.Println("Injecting TX")
	ctx := context.Background()
	queryy := bson.M{
		"id": offTx.Id,
	}
	findResult := e.FindOne(ctx, bson.M{
		"id": offTx.Id,
	})
	opts := options.FindOneAndUpdate()
	opts.SetUpsert(true)
	fmt.Println(findResult, findResult.Err())
	setOp := bson.M{
		"status":          "INCLUDED",
		"first_seen":      time.Now(),
		"a_block":         offTx.AnchoredBlock,
		"anchored_id":     offTx.AnchoredId,
		"anchored_height": offTx.AnchoredHeight,
		"anchored_index":  offTx.AnchoredIndex,
		"anchored_opidx":  offTx.AnchoredOpIdx,
		"data":            offTx.Tx,
		"required_auths":  offTx.RequiredAuths,
	}
	if findResult.Err() != nil {
		setOp["first_seen"] = time.Now()
		//Prevents case of reprocessing/reindexing
		setOp["status"] = "INCLUDED"
	} else {
		//If it already exists do nothing
	}
	updateResult := e.FindOneAndUpdate(ctx, queryy, bson.M{
		"$set": setOp,
	}, opts)

	return updateResult.Err()
}
