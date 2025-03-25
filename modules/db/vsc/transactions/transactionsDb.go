package transactions

import (
	"context"
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
	ctx := context.Background()

	queryy := bson.M{
		"id": offTx.Id,
	}

	findResult := e.FindOne(ctx, bson.M{
		"id": offTx.Id,
	})

	opts := options.FindOneAndUpdate().SetUpsert(true)
	setOp := bson.M{
		"anchr_block":    offTx.AnchoredBlock,
		"anchr_id":       offTx.AnchoredId,
		"anchr_height":   offTx.AnchoredHeight,
		"anchr_index":    offTx.AnchoredIndex,
		"anchr_opidx":    offTx.AnchoredOpIdx,
		"data":           offTx.Tx,
		"required_auths": offTx.RequiredAuths,
		"nonce":          offTx.Nonce,
	}
	if findResult.Err() != nil {
		setOp["first_seen"] = time.Now()
		//Prevents case of reprocessing/reindexing
		if offTx.Status != "" {
			setOp["status"] = offTx.Status
		} else {
			setOp["status"] = "UNCONFIRMED"
		}
	} else {
		//If it already exists do nothing
		if offTx.Status != "" {
			setOp["status"] = offTx.Status
		}
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

// Searches for unconfirmed VSC transactions with no verification
// Provide height for expiration filtering
func (e *transactions) FindUnconfirmedTransactions(height uint64) ([]TransactionRecord, error) {
	query := bson.M{
		"status": "UNCONFIRMED",
		"$or": bson.A{
			bson.M{
				"expire_block": bson.M{
					"$exists": false,
				},
			},
			bson.M{
				"expire_block": bson.M{
					"$gt": height,
				},
			},
			bson.M{
				"expire_block": bson.M{
					"$eq": nil,
				},
			},
		},
	}

	ctx := context.Background()
	findResult, _ := e.Find(ctx, query)

	txList := make([]TransactionRecord, 0)
	for findResult.Next(ctx) {
		tx := TransactionRecord{}
		err := findResult.Decode(&tx)

		if err != nil {
			return nil, err
		}
		txList = append(txList, tx)
	}

	return txList, nil
}
