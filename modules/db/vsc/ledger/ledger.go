package ledgerDb

import (
	"context"
	"fmt"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type ledger struct {
	*db.Collection
}

func New(d *vsc.VscDb) Ledger {
	return &ledger{db.NewCollection(d.DbInstance, "ledger")}
}

func (ledger *ledger) StoreLedger(ledgerRecord LedgerRecord) {
	findUpdateOpts := options.FindOneAndUpdate().SetUpsert(true)
	ledger.FindOneAndUpdate(context.Background(), bson.M{
		"id": ledgerRecord.Id,
	}, bson.M{
		"$set": ledgerRecord,
	}, findUpdateOpts)
}

// Get ledger ops after height inclusive
func (ledger *ledger) GetLedgerAfterHeight(account string, blockHeight int64, asset string, limit *int64) (*[]LedgerRecord, error) {
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
	fmt.Println("Results: ", results)

	return &results, nil
	// return nil
}

type balances struct {
	*db.Collection
}

func NewBalances(d *vsc.VscDb) Balances {
	return &balances{db.NewCollection(d.DbInstance, "balances")}
}

// Gets the balance record for a given account and asset
// Note: this does not return updated ledger records
func (balances *balances) GetBalanceRecord(account string, blockHeight int64, asset string) (int64, error) {
	singleResult := balances.FindOne(context.Background(), bson.M{
		"account": account,
		"block_height": bson.M{
			"$lt": blockHeight,
		},
	})

	if singleResult.Err() != nil {
		return 0, singleResult.Err()
	}
	balRecord := BalanceRecord{}
	singleResult.Decode(&balRecord)
	return 0, nil
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
