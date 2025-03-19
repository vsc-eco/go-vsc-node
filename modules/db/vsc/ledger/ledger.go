package ledgerDb

import (
	"context"
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
func (ledger *ledger) GetLedgerRange(account string, start uint64, end uint64, asset string) (*[]LedgerRecord, error) {
	opts := options.Find().SetSort(bson.M{"block_height": 1})

	findResult, err := ledger.Find(context.Background(), bson.M{
		"owner": account,
		"block_height": bson.M{
			"$gte": start,
			"$lte": end,
		},
		"tk": asset,
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
}

func (ledger *ledger) GetDistinctAccountsRange(startHeight, endHeight uint64) ([]string, error) {
	arr, err := ledger.Distinct(context.Background(), "owner", bson.M{
		// "block_height": bson.M{
		//Example: 21
		// "$gte": startHeight,
		// //Example: 30
		// "$lte": endHeight,
		//Captures range of 21 - 30 (inclusive)
		// },
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
	return &balances{db.NewCollection(d.DbInstance, "balances")}
}

// Gets the balance record for a given account and asset
// Note: this does not return updated ledger records
func (balances *balances) GetBalanceRecord(account string, blockHeight uint64, asset string) (*BalanceRecord, error) {
	singleResult := balances.FindOne(context.Background(), bson.M{
		"account": account,
		"block_height": bson.M{
			"$lte": blockHeight,
		},
	})

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
func (balances *balances) UpdateBalanceRecord(account string, blockHeight uint64, balancesMap map[string]int64) error {
	findUpdateOpts := options.FindOneAndUpdate().SetUpsert(true)
	balances.FindOneAndUpdate(context.Background(), bson.M{
		"account":      account,
		"block_height": blockHeight,
	}, bson.M{
		"$set": balancesMap,
	}, findUpdateOpts)
	return nil
}

func (balances *balances) GetAll(blockHeight uint64) []BalanceRecord {
	return nil
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

func (actionsDb *actionsDb) ExecuteComplete(id string) {
	actionsDb.FindOneAndUpdate(context.Background(), bson.M{
		"id": id,
	}, bson.M{
		"$set": bson.M{
			"status": "complete",
		},
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

func (actionsDb *actionsDb) GetPendingActions(bh uint64) ([]ActionRecord, error) {
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
	cursor, err := actionsDb.Find(context.Background(), bson.M{
		"status": "pending",
		"block_height": bson.M{
			"$lte": bh,
		},
	}, options)

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

type interestClaims struct {
	*db.Collection
}

func (ic *interestClaims) GetLastClaim(blockHeight uint64) *ClaimRecord {
	findResult := ic.FindOne(context.Background(), bson.M{
		"block_height": bson.M{
			"$lt": blockHeight,
		},
	})
	if findResult.Err() != nil {
		return nil
	}
	claimRecord := ClaimRecord{}
	findResult.Decode(&claimRecord)
	return &claimRecord
}

func (ic *interestClaims) SaveClaim(blockHeight uint64, amount int64) {
	claimRecord := ClaimRecord{
		BlockHeight: blockHeight,
		Amount:      amount,
	}
	options := options.FindOneAndUpdate().SetUpsert(true)
	ic.FindOneAndUpdate(context.Background(), bson.M{
		"block_height": blockHeight,
	}, bson.M{
		"$set": claimRecord,
	}, options)
}

func NewInterestClaimDb(d *vsc.VscDb) InterestClaims {
	return &interestClaims{db.NewCollection(d.DbInstance, "ledger_claims")}
}
