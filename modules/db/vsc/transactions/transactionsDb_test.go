package transactions_test

import (
	"context"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/transactions"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
)

// testAnchorHeight is the hive block height every test transaction is anchored
// to. setupTxDB seeds a matching hive_blocks document so the timestamp
// $lookup/$unwind in FindTransactions resolves (otherwise the unwind, which
// does not preserve null/empty arrays, drops the transaction).
const testAnchorHeight uint64 = 100

func u64(v uint64) *uint64 { return &v }

// setupTxDB boots an isolated test transaction_pool collection, seeds one
// hive_blocks document at testAnchorHeight, and returns the Transactions
// plugin plus the raw mongo handles (so later tests can insert legacy-shaped
// documents directly via
// dbi.Database(dbConfig.GetDbName()).Collection("transaction_pool")).
func setupTxDB(t *testing.T) (transactions.Transactions, db.Db, db.DbConfig) {
	config.UseMainConfigDuringTests = true
	dbConfig := db.NewDbConfig()
	dbConfig.Init()
	dbConfig.SetDbName("go-vsc-tx-test")
	dbi := db.New(dbConfig)
	v := vsc.New(dbi, dbConfig)
	tx := transactions.New(v)
	hb, err := hive_blocks.New(v)
	assert.NoError(t, err)

	test_utils.RunPlugin(t, aggregate.New([]aggregate.Plugin{
		dbConfig, dbi, v, tx, hb,
	}))
	err = v.Clear()
	assert.NoError(t, err)

	err = hb.StoreBlocks(testAnchorHeight, hive_blocks.HiveBlock{
		BlockNumber: testAnchorHeight,
		BlockID:     "test-blk-100",
		Timestamp:   "2026-05-18T00:00:00",
	})
	assert.NoError(t, err)
	return tx, dbi, dbConfig
}

// TestInitIdempotent asserts the payload_recipients index is created by Init
// and that a second Init does not error (CreateIndexIfNotExist).
func TestInitIdempotent(t *testing.T) {
	tx, dbi, dbConfig := setupTxDB(t)
	assert.NoError(t, tx.Init()) // second call must be a no-op, not an error

	coll := dbi.Database(dbConfig.GetDbName()).Collection("transaction_pool")
	specs, err := coll.Indexes().ListSpecifications(context.Background())
	assert.NoError(t, err)
	found := false
	for _, s := range specs {
		if s.Name == "payload_recipients_1" {
			found = true
		}
	}
	assert.True(t, found, "payload_recipients index must exist after Init")
}

func TestInboundContractTransferVisibleToRecipient(t *testing.T) {
	tx, _, _ := setupTxDB(t)

	err := tx.Ingest(transactions.IngestTransactionUpdate{
		Id:             "tx-call-1",
		Status:         "CONFIRMED",
		Type:           "call",
		RequiredAuths:  []string{"hive:devser.v4vapp"},
		AnchoredHeight: u64(testAnchorHeight),
		Ops: []transactions.TransactionOperation{
			{
				Type: "call",
				Data: map[string]interface{}{
					"contract_id": "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d",
					"action":      "transfer",
					"payload":     `{"amount":"432","to":"hive:v4vapp-test"}`,
				},
			},
		},
	})
	assert.NoError(t, err)

	// Recipient (in payload, NOT in required_auths) must see the tx.
	acct := "hive:v4vapp-test"
	res, err := tx.FindTransactions(nil, nil, &acct, nil, nil, nil, nil, nil, nil, nil, 0, 50)
	assert.NoError(t, err)
	assert.Len(t, res, 1)
	if len(res) == 1 {
		assert.Equal(t, "tx-call-1", res[0].Id)
	}

	// Sender still finds it via required_auths (no regression).
	sender := "hive:devser.v4vapp"
	res, err = tx.FindTransactions(nil, nil, &sender, nil, nil, nil, nil, nil, nil, nil, 0, 50)
	assert.NoError(t, err)
	assert.Len(t, res, 1)

	// Unrelated account sees nothing.
	other := "hive:nobody"
	res, err = tx.FindTransactions(nil, nil, &other, nil, nil, nil, nil, nil, nil, nil, 0, 50)
	assert.NoError(t, err)
	assert.Len(t, res, 0)
}

func TestLegacyInboundTransferVisibleViaRegexFallback(t *testing.T) {
	tx, dbi, dbConfig := setupTxDB(t)

	// Simulate a document ingested by OLD code: no payload_recipients
	// field, recipient only present inside the ops.data.payload string.
	// anchr_height matches the seeded hive block so the timestamp pipeline
	// does not drop it.
	coll := dbi.Database(dbConfig.GetDbName()).Collection("transaction_pool")
	_, err := coll.InsertOne(context.Background(), bson.M{
		"id":             "tx-legacy-1",
		"status":         "CONFIRMED",
		"type":           "call",
		"required_auths": bson.A{"hive:devser.v4vapp"},
		"anchr_height":   testAnchorHeight,
		"ops": bson.A{bson.M{
			"type": "call",
			"data": bson.M{
				"contract_id": "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d",
				"action":      "transfer",
				"payload":     `{"amount":"99","to":"hive:v4vapp-test"}`,
			},
		}},
		// NOTE: deliberately no "payload_recipients" key.
	})
	assert.NoError(t, err)

	// Recipient must still be found, via the regex fallback.
	acct := "hive:v4vapp-test"
	res, err := tx.FindTransactions(nil, nil, &acct, nil, nil, nil, nil, nil, nil, nil, 0, 50)
	assert.NoError(t, err)
	assert.Len(t, res, 1)
	if len(res) == 1 {
		assert.Equal(t, "tx-legacy-1", res[0].Id)
	}

	// Prefix safety: a longer account name must not match this doc.
	bobby := "hive:v4vapp-testxyz"
	res, err = tx.FindTransactions(nil, nil, &bobby, nil, nil, nil, nil, nil, nil, nil, 0, 50)
	assert.NoError(t, err)
	assert.Len(t, res, 0)
}
