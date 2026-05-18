package transactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
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

	// Indexes inbound contract-transfer recipients extracted at ingest
	// (see Ingest). Sparse: the vast majority of txs have no contract
	// recipients. Non-unique: many txs can credit the same account.
	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "payload_recipients", Value: 1}},
		Options: options.Index().SetSparse(true),
	}
	if err = e.CreateIndexIfNotExist(indexModel); err != nil {
		return fmt.Errorf("failed to create payload_recipients index: %w", err)
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

	opts := options.Update().SetUpsert(true)
	setOp := bson.M{
		"anchr_height":           offTx.AnchoredHeight,
		"anchr_block":            offTx.AnchoredBlock,
		"anchr_index":            offTx.AnchoredIndex,
		"anchr_id":               offTx.AnchoredId,
		"type":                   offTx.Type,
		"ops":                    offTx.Ops,
		"op_types":               offTx.OpTypes,
		"required_auths":         offTx.RequiredAuths,
		"required_posting_auths": offTx.RequiredPostingAuths,
		"nonce":                  offTx.Nonce,
		"rc_limit":               offTx.RcLimit,
		"ledger":                 offTx.Ledger,
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

	// Extract recipients buried in contract-call payloads (e.g. sats/token
	// transfers). The recipient lives inside ops.data.payload as a JSON
	// string, unreachable by Mongo equality; surface it as an indexed field
	// so FindTransactions can match inbound contract transfers.
	payloadRecipients := []string{}
	for _, op := range offTx.Ops {
		if op.Type != "call" {
			continue
		}
		pl, ok := op.Data["payload"].(string)
		if !ok {
			continue
		}
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(pl), &parsed) != nil {
			continue
		}
		if to, ok := parsed["to"].(string); ok && to != "" {
			payloadRecipients = append(payloadRecipients, to)
		}
	}
	setOp["payload_recipients"] = payloadRecipients

	_, err := e.UpdateOne(ctx, queryy, bson.M{
		"$set": setOp,
	}, opts)

	return err
}

func (e *transactions) SetOutput(sOut SetResultUpdate) {
	query := bson.M{
		"id": sOut.Id,
	}
	ctx := context.Background()

	update := bson.M{}
	push := bson.M{}

	if sOut.Output != nil {
		push["output"] = sOut.Output
	}
	if sOut.Ledger != nil {
		update["ledger"] = sOut.Ledger
	}
	if sOut.Status != nil {
		update["status"] = sOut.Status
	}

	e.UpdateOne(ctx, query, bson.M{
		"$set":  update,
		"$push": push,
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

func (e *transactions) FindTransactions(ids []string, id *string, account *string, contract *string, status *TransactionStatus, byType []string, ledgerToFrom *string, ledgerTypes []string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]TransactionRecord, error) {
	if id != nil && ids != nil {
		return nil, errors.New("either input a single id or a list of ids")
	}
	filters := bson.D{}
	if id != nil {
		filters = append(filters, bson.E{Key: "id", Value: *id})
	}
	if ids != nil {
		filters = append(filters, bson.E{Key: "id", Value: bson.D{{Key: "$in", Value: ids}}})
	}
	if account != nil {
		filters = append(filters, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "required_auths", Value: *account}},
			bson.D{{Key: "required_posting_auths", Value: *account}},
			bson.D{{Key: "ops.data.to", Value: *account}},
			// Inbound contract transfers: recipient extracted at ingest.
			bson.D{{Key: "payload_recipients", Value: *account}},
		}})
	}
	if contract != nil {
		filters = append(filters, bson.E{Key: "ops.data.contract_id", Value: *contract})
	}
	if status != nil {
		filters = append(filters, bson.E{Key: "status", Value: string(*status)})
	}
	if byType != nil {
		filters = append(filters, bson.E{Key: "op_types", Value: bson.M{
			"$in": byType,
		}})
	}
	if ledgerToFrom != nil {
		filters = append(filters, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "ledger.from", Value: *ledgerToFrom}},
			bson.D{{Key: "ledger.to", Value: *ledgerToFrom}},
		}})
	}
	if len(ledgerTypes) > 0 {
		ledgerTypeFilter := bson.A{}
		for _, t := range ledgerTypes {
			ledgerTypeFilter = append(ledgerTypeFilter, bson.D{{Key: "ledger.type", Value: t}})
		}
		filters = append(filters, bson.E{Key: "$or", Value: ledgerTypeFilter})
	}
	if fromBlock != nil {
		filters = append(filters, bson.E{Key: "anchr_height", Value: bson.D{{Key: "$gte", Value: *fromBlock}}})
	}
	if toBlock != nil {
		filters = append(filters, bson.E{Key: "anchr_height", Value: bson.D{{Key: "$lte", Value: *toBlock}}})
	}
	pipe := hive_blocks.GetAggTimestampPipeline2(filters, "anchr_height", "anchr_ts", offset, limit)
	cursor, err := e.Aggregate(context.TODO(), pipe)
	if err != nil {
		return []TransactionRecord{}, err
	}
	defer cursor.Close(context.TODO())
	var results []TransactionRecord
	for cursor.Next(context.TODO()) {
		var elem TransactionRecord
		if err := cursor.Decode(&elem); err != nil {
			return []TransactionRecord{}, err
		}
		results = append(results, elem)
	}
	return results, nil
}

// InvalidateCompetingTransactions deletes UNCONFIRMED transactions
// that share the same required_auths and nonce as a confirmed transaction.
func (e *transactions) InvalidateCompetingTransactions(requiredAuths []string, nonces []uint64) (int64, error) {
	filter := bson.M{
		"status": string(TransactionStatusUnconfirmed),
		"required_auths": bson.M{
			"$all":  requiredAuths,
			"$size": len(requiredAuths),
		},
		"nonce": bson.M{"$in": nonces},
	}

	result, err := e.DeleteMany(context.Background(), filter)
	if err != nil {
		return 0, err
	}
	return result.DeletedCount, nil
}

// HasUnconfirmedWithNonce checks if an UNCONFIRMED transaction exists
// with the given required_auths and nonce.
func (e *transactions) HasUnconfirmedWithNonce(requiredAuths []string, nonce uint64) (bool, error) {
	filter := bson.M{
		"status": string(TransactionStatusUnconfirmed),
		"required_auths": bson.M{
			"$all":  requiredAuths,
			"$size": len(requiredAuths),
		},
		"nonce": nonce,
	}

	count, err := e.CountDocuments(context.Background(), filter)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// Searches for unconfirmed VSC transactions with no verification
// Provide height for expiration filtering
func (e *transactions) FindUnconfirmedTransactions(height uint64) ([]TransactionRecord, error) {
	query := bson.M{
		"status": "UNCONFIRMED",
		"type":   "vsc",
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
