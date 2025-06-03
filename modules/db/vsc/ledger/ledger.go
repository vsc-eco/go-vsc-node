package ledgerDb

import (
	"context"
	"strings"
	"vsc-node/modules/common"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type ledger struct {
	*db.Collection
}

func New(d *vsc.VscDb) Ledger {
	return &ledger{db.NewCollection(d.DbInstance, "ledger")}
}

func (ledger *ledger) StoreLedger(ledgerRecords ...LedgerRecord) {
	if len(ledgerRecords) > 0 {
		for _, ledgerRecord := range ledgerRecords {
			findUpdateOpts := options.FindOneAndUpdate().SetUpsert(true)
			ledger.FindOneAndUpdate(context.Background(), bson.M{
				"id": ledgerRecord.Id,
			}, bson.M{
				"$set": ledgerRecord,
			}, findUpdateOpts)
		}
	}
}

// Get ledger ops after height inclusive
func (ledger *ledger) GetLedgerAfterHeight(account string, blockHeight uint64, asset string, limit *int64) (*[]LedgerRecord, error) {
	opts := options.Find().SetSort(bson.M{"block_height": 1})
	if limit != nil {
		opts.SetLimit(*limit)
	}
	findResult, err := ledger.Find(context.Background(), bson.M{
		"owner": account,
		"block_height": bson.M{
			"$gte": blockHeight,
		},
	}, opts)
	if err != nil {
		return nil, err
	}

	results := make([]LedgerRecord, 0)
	for findResult.Next(context.Background()) {
		ledRes := LedgerRecord{}
		findResult.Decode(&ledRes)
		results = append(results, ledRes)
	}

	return &results, nil
	// return nil
}

// Get ledger ops after height inclusive
func (ledger *ledger) GetLedgerRange(account string, start uint64, end uint64, asset string, searchOps ...LedgerOptions) (*[]LedgerRecord, error) {
	opts := options.Find().SetSort(bson.M{"block_height": 1})

	query := bson.M{
		"owner": account,
		"block_height": bson.M{
			"$gte": start,
			"$lte": end,
		},
	}
	if asset != "" {
		query["tk"] = asset
	}

	for _, op := range searchOps {
		if len(op.OpType) > 0 {
			query["t"] = bson.M{
				"$in": op.OpType,
			}
		}
	}
	findResult, err := ledger.Find(context.Background(), query, opts)
	if err != nil {
		return nil, err
	}

	results := make([]LedgerRecord, 0)
	for findResult.Next(context.Background()) {
		ledRes := LedgerRecord{}
		findResult.Decode(&ledRes)
		results = append(results, ledRes)
	}

	return &results, nil
}

func (ledger *ledger) GetLedgersTsRange(account *string, txId *string, txTypes []string, asset *Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]LedgerRecord, error) {
	filters := bson.D{}
	if account != nil {
		filters = append(filters, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "from", Value: *account}},
			bson.D{{Key: "owner", Value: *account}},
		}})
	}
	if txId != nil {
		filters = append(filters, bson.E{Key: "id", Value: bson.D{{Key: "$regex", Value: "^" + (*txId)}}})
	}
	if fromBlock != nil {
		filters = append(filters, bson.E{Key: "block_height", Value: bson.D{{Key: "$gte", Value: *fromBlock}}})
	}
	if toBlock != nil {
		filters = append(filters, bson.E{Key: "block_height", Value: bson.D{{Key: "$lte", Value: *toBlock}}})
	}
	if len(txTypes) > 0 {
		filters = append(filters, bson.E{Key: "t", Value: bson.D{{Key: "$in", Value: txTypes}}})
	}
	if asset != nil {
		filters = append(filters, bson.E{Key: "tk", Value: string(*asset)})
	}
	pipe := hive_blocks.GetAggTimestampPipeline(filters, "block_height", "timestamp", offset, limit)
	cursor, err := ledger.Aggregate(context.TODO(), pipe)
	if err != nil {
		return []LedgerRecord{}, err
	}
	defer cursor.Close(context.TODO())
	var results []LedgerRecord
	for cursor.Next(context.TODO()) {
		var elem LedgerRecord
		if err := cursor.Decode(&elem); err != nil {
			return []LedgerRecord{}, err
		}
		results = append(results, elem)
	}
	return results, nil
}

func (ledger *ledger) GetDistinctAccountsRange(startHeight, endHeight uint64) ([]string, error) {
	arr, err := ledger.Distinct(context.Background(), "owner", bson.M{
		"block_height": bson.M{
			//Example: 21
			"$gte": startHeight,
			//Example: 30
			"$lte": endHeight,
			//Captures range of 21 - 30 (inclusive)
		},
	})

	if err != nil {
		return nil, err
	}

	accounts := make([]string, 0)
	for _, v := range arr {
		accounts = append(accounts, v.(string))
	}

	return accounts, nil
}

type balances struct {
	*db.Collection
}

func NewBalances(d *vsc.VscDb) Balances {
	return &balances{db.NewCollection(d.DbInstance, "ledger_balances")}
}

// Gets the balance record for a given account and asset
// Note: this does not return updated ledger records
func (balances *balances) GetBalanceRecord(account string, blockHeight uint64) (*BalanceRecord, error) {
	options := options.FindOne().SetSort(bson.D{{"block_height", -1}})
	singleResult := balances.FindOne(context.Background(), bson.M{
		"account": account,
		"block_height": bson.M{
			"$lte": blockHeight,
		},
	}, options)

	if singleResult.Err() != nil {
		if singleResult.Err().Error() == "mongo: no documents in result" {
			return nil, nil
		}
		return nil, singleResult.Err()
	}
	balRecord := BalanceRecord{}
	singleResult.Decode(&balRecord)

	return &balRecord, nil
}

func (balances *balances) PutBalanceRecord(balRecord BalanceRecord) {
	findUpdateOpts := options.FindOneAndUpdate().SetUpsert(true)
	balances.FindOneAndUpdate(context.Background(), bson.M{
		"account":      balRecord.Account,
		"block_height": balRecord.BlockHeight,
	}, bson.M{
		"$set": balRecord,
	}, findUpdateOpts)
}

// FIX ME!!
func (balances *balances) UpdateBalanceRecord(record BalanceRecord) error {
	findUpdateOpts := options.FindOneAndUpdate().SetUpsert(true)
	balances.FindOneAndUpdate(context.Background(), bson.M{
		"account":      record.Account,
		"block_height": record.BlockHeight,
	}, bson.M{
		"$set": record,
	}, findUpdateOpts)
	return nil
}

func (balances *balances) GetAll(blockHeight uint64) []BalanceRecord {
	distinctAccountZ, _ := balances.Distinct(context.Background(), "account", bson.M{})
	distinctAccount := common.ArrayToStringArray(distinctAccountZ)

	//TODO: Either use a bulk read or use threads
	//Initial iteration
	records := make([]BalanceRecord, 0)
	for _, act := range distinctAccount {
		br, _ := balances.GetBalanceRecord(act, blockHeight)

		if br != nil {
			records = append(records, *br)
		}
	}

	return records
}

type actionsDb struct {
	*db.Collection
}

func NewActionsDb(d *vsc.VscDb) BridgeActions {
	return &actionsDb{db.NewCollection(d.DbInstance, "ledger_actions")}
}

func (actionsDb *actionsDb) StoreAction(withdraw ActionRecord) {
	findUpdateOpts := options.FindOneAndUpdate().SetUpsert(true)
	actionsDb.FindOneAndUpdate(context.Background(), bson.M{
		"id": withdraw.Id,
	}, bson.M{
		"$set": withdraw,
	}, findUpdateOpts)
}

func (actionsDb *actionsDb) ExecuteComplete(actionId *string, ids ...string) {
	if len(ids) == 0 {
		return
	}
	updated := bson.M{
		"status": "complete",
	}
	if actionId != nil {
		updated["action_id"] = *actionId
	}
	actionsDb.UpdateMany(context.Background(), bson.M{
		"id": bson.M{
			"$in": ids,
		},
	}, bson.M{
		"$set": updated,
	})
}

func (actionsDb *actionsDb) Get(id string) (*ActionRecord, error) {
	findResult := actionsDb.FindOne(context.Background(), bson.M{
		"id": id,
	})

	if findResult.Err() != nil {
		return nil, findResult.Err()
	}

	ac := ActionRecord{}

	findResult.Decode(&ac)

	return &ac, nil
}

func (actionsDb *actionsDb) SetStatus(id string, status string) {

}

func (actionsDb *actionsDb) GetPendingActions(bh uint64, t ...string) ([]ActionRecord, error) {
	options := options.Find().SetSort(bson.D{
		{
			Key:   "block_height",
			Value: 1,
		},
		{
			Key:   "id",
			Value: 1,
		},
	})
	query := bson.M{
		"status": "pending",
		"block_height": bson.M{
			"$lte": bh,
		},
	}
	if len(t) > 0 {
		query["type"] = bson.M{
			"$in": t,
		}
	}
	cursor, err := actionsDb.Find(context.Background(), query, options)

	if err != nil {
		return nil, err
	}

	results := make([]ActionRecord, 0)
	for cursor.Next(context.Background()) {
		action := ActionRecord{}
		cursor.Decode(&action)
		results = append(results, action)
	}

	return results, nil
}

// Gets list of actions equal or less than the supplied epoch
func (actions *actionsDb) GetPendingActionsByEpoch(epoch uint64, t ...string) ([]ActionRecord, error) {
	options := options.Find().SetSort(bson.D{
		{
			Key:   "block_height",
			Value: 1,
		},
		{
			Key:   "id",
			Value: 1,
		},
	})
	query := bson.M{
		"data.epoch": bson.M{
			"$lte":    epoch,
			"$exists": true,
		},
		"status": "pending",
	}
	if len(t) > 0 {
		query["type"] = bson.M{
			"$in": t,
		}
	}
	cursor, _ := actions.Find(context.Background(), query, options)

	actionRecords := make([]ActionRecord, 0)

	if cursor.Next(context.Background()) {
		record := ActionRecord{}
		cursor.Decode(&record)

		actionRecords = append(actionRecords, record)
	}

	return actionRecords, nil
}

func (actions *actionsDb) GetActionsRange(txId *string, actionId *string, account *string, byTypes []string, asset *Asset, status *string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ActionRecord, error) {
	filters := bson.D{}
	if txId != nil {
		filters = append(filters, bson.E{Key: "id", Value: *txId})
	}
	if actionId != nil {
		filters = append(filters, bson.E{Key: "action_id", Value: *actionId})
	}
	if account != nil {
		filters = append(filters, bson.E{Key: "to", Value: *account})
	}
	if len(byTypes) > 0 {
		filters = append(filters, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: byTypes}}})
	}
	if asset != nil {
		filters = append(filters, bson.E{Key: "asset", Value: string(*asset)})
	}
	if status != nil {
		filters = append(filters, bson.E{Key: "status", Value: strings.ToLower(*status)})
	}
	if fromBlock != nil {
		filters = append(filters, bson.E{Key: "block_height", Value: bson.D{{Key: "$gte", Value: *fromBlock}}})
	}
	if toBlock != nil {
		filters = append(filters, bson.E{Key: "block_height", Value: bson.D{{Key: "$lte", Value: *toBlock}}})
	}
	pipe := hive_blocks.GetAggTimestampPipeline(filters, "block_height", "timestamp", offset, limit)
	cursor, err := actions.Aggregate(context.TODO(), pipe)
	if err != nil {
		return []ActionRecord{}, err
	}
	defer cursor.Close(context.TODO())
	var results []ActionRecord
	for cursor.Next(context.TODO()) {
		var elem ActionRecord
		if err := cursor.Decode(&elem); err != nil {
			return []ActionRecord{}, err
		}
		results = append(results, elem)
	}
	return results, nil
}

func (actions *actionsDb) GetAccountPendingConsensusUnstake(account string) (int64, error) {
	cursor, err := actions.Aggregate(context.TODO(), mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "to", Value: account},
			{Key: "status", Value: "pending"},
			{Key: "type", Value: "consensus_unstake"},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "totalAmount", Value: bson.M{"$sum": "$amount"}},
		}}},
	})
	if err != nil {
		return -1, err
	}
	defer cursor.Close(context.TODO())

	var result struct {
		TotalAmount int64 `bson:"totalAmount"`
	}
	if cursor.Next(context.TODO()) {
		if err := cursor.Decode(&result); err != nil {
			return -1, err
		}
		return result.TotalAmount, nil
	}
	return 0, nil
}

func (actions *actionsDb) GetActionsByTxId(txId string) ([]ActionRecord, error) {
	cursor, err := actions.Find(context.Background(), bson.M{
		"id": bson.M{
			"$regex": "^" + txId,
		},
	})
	if err != nil {
		return nil, err
	}

	actionRecords := make([]ActionRecord, 0)

	for cursor.Next(context.Background()) {
		record := ActionRecord{}
		cursor.Decode(&record)

		actionRecords = append(actionRecords, record)
	}

	return actionRecords, nil
}

type interestClaims struct {
	*db.Collection
}

func (ic *interestClaims) GetLastClaim(blockHeight uint64) *ClaimRecord {
	options := options.FindOne().SetSort(bson.D{{"block_height", -1}})
	findResult := ic.FindOne(context.Background(), bson.M{
		"block_height": bson.M{
			"$lt": blockHeight,
		},
	}, options)
	if findResult.Err() != nil {
		return nil
	}
	claimRecord := ClaimRecord{}
	findResult.Decode(&claimRecord)
	return &claimRecord
}

func (ic *interestClaims) SaveClaim(claim ClaimRecord) {

	options := options.FindOneAndUpdate().SetUpsert(true)
	ic.FindOneAndUpdate(context.Background(), bson.M{
		"block_height": claim.BlockHeight,
	}, bson.M{
		"$set": claim,
	}, options)
}

func NewInterestClaimDb(d *vsc.VscDb) InterestClaims {
	return &interestClaims{db.NewCollection(d.DbInstance, "ledger_claims")}
}
