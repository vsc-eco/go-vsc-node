package transactions_test

import (
	"context"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/transactions"

	"github.com/stretchr/testify/assert"
)

// setupTxDB boots an isolated test transaction_pool collection and returns
// the Transactions plugin plus the raw mongo handles, so later tests can
// insert legacy-shaped documents directly via
// dbi.Database(dbConfig.GetDbName()).Collection("transaction_pool").
func setupTxDB(t *testing.T) (transactions.Transactions, db.Db, db.DbConfig) {
	config.UseMainConfigDuringTests = true
	dbConfig := db.NewDbConfig()
	dbConfig.Init()
	dbConfig.SetDbName("go-vsc-tx-test")
	dbi := db.New(dbConfig)
	v := vsc.New(dbi, dbConfig)
	tx := transactions.New(v)

	test_utils.RunPlugin(t, aggregate.New([]aggregate.Plugin{
		dbConfig, dbi, v, tx,
	}))
	err := v.Clear()
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
