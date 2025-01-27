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
	fmt.Println("Ingesting TX")
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
		"first_seen":     time.Now(),
		"anchr_block":    offTx.AnchoredBlock,
		"anchr_id":       offTx.AnchoredId,
		"anchr_height":   offTx.AnchoredHeight,
		"anchr_index":    offTx.AnchoredIndex,
		"anchr_opidx":    offTx.AnchoredOpIdx,
		"data":           offTx.Tx,
		"required_auths": offTx.RequiredAuths,
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

func (e *transactions) SetOutput(sOut SetOutputUpdate) {
	query := bson.M{
		"id": sOut.Id,
	}
	ctx := context.Background()

	e.FindOneAndUpdate(ctx, query, bson.M{
		"$set": bson.M{
			"output": bson.M{
				"id":    sOut.OutputId,
				"index": sOut.Index,
			},
		},
	})
}

func (e *transactions) GetTransaction(id string) *TransactionRecord {
	query := bson.M{
		"id": id,
	}
	ctx := context.Background()
	findResult := e.FindOne(ctx, query)

	if findResult.Err() != nil {
		return nil
	}
	record := TransactionRecord{}
	err := findResult.Decode(&record)
	if err != nil {
		return nil
	}
	return &record
}
