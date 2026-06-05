package contracts_test

import (
	"context"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/hive_blocks"
	wasm_runtime "vsc-node/modules/wasm/runtime"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
)

// These tests exercise the contract-update timelock at the DB layer: a queued
// update (a version whose activation_height is in the future) must stay invisible
// to ContractById — the chokepoint every code-execution path uses — until the
// query height reaches its activation, and a cancelled update must never activate.

const (
	createHeight = uint64(100)
	updateHeight = uint64(150)
	// Activation 30 blocks after the update was submitted (mirrors the testnet
	// timelock); the DB layer is told the activation height explicitly — the
	// duration policy lives in the state engine, not here.
	activateHeight = uint64(180)
)

func setupContractsDB(t *testing.T) (contracts.Contracts, hive_blocks.HiveBlocks, db.Db, db.DbConfig) {
	config.UseMainConfigDuringTests = true
	dbConfig := db.NewDbConfig()
	dbConfig.Init()
	dbConfig.SetDbName("go-vsc-contracts-test")
	dbi := db.New(dbConfig)
	v := vsc.New(dbi, dbConfig)
	c := contracts.New(v)
	hb, err := hive_blocks.New(v)
	assert.NoError(t, err)

	test_utils.RunPlugin(t, aggregate.New([]aggregate.Plugin{
		dbConfig, dbi, v, c, hb,
	}))
	err = v.Clear()
	assert.NoError(t, err)
	return c, hb, dbi, dbConfig
}

// seedBlocks stores hive_blocks docs so the creation_ts $lookup/$unwind in
// FindActiveContracts/FindPendingUpdates resolves (the unwind drops rows with no
// matching block). Activation heights are NOT joined, so they need no block.
func seedBlocks(t *testing.T, hb hive_blocks.HiveBlocks, heights ...uint64) {
	for _, h := range heights {
		err := hb.StoreBlocks(h, hive_blocks.HiveBlock{
			BlockNumber: h,
			BlockID:     "blk",
			Timestamp:   "2026-06-05T00:00:00",
		})
		assert.NoError(t, err)
	}
}

// registerWithTimelock seeds a contract created at createHeight and a code update
// queued at updateHeight that activates at activateHeight.
func registerWithTimelock(c contracts.Contracts) string {
	id := "vsc1timelocktest"
	c.RegisterContract(id, contracts.Contract{
		Code:             "codeA",
		Name:             "test",
		Creator:          "hive:alice",
		Owner:            "hive:alice",
		Proposer:         "hive:alice",
		TxId:             "tx-create",
		CreationHeight:   createHeight,
		ActivationHeight: createHeight,
		Runtime:          wasm_runtime.Go,
	})
	c.RegisterContract(id, contracts.Contract{
		Code:             "codeB",
		Name:             "test",
		Creator:          "hive:alice",
		Owner:            "hive:alice",
		Proposer:         "hive:alice",
		TxId:             "tx-update",
		CreationHeight:   updateHeight,
		ActivationHeight: activateHeight,
		Runtime:          wasm_runtime.Go,
	})
	return id
}

// A queued code update does not become the active code until its activation
// height; the previously-active code keeps running for the whole window.
func TestContractById_TimelockGatesActivation(t *testing.T) {
	c, hb, _, _ := setupContractsDB(t)
	seedBlocks(t, hb, createHeight, updateHeight)
	id := registerWithTimelock(c)

	cases := []struct {
		height   uint64
		wantCode string
	}{
		{createHeight, "codeA"}, // right after deploy
		{updateHeight, "codeA"}, // update just submitted — still old code
		{updateHeight + 1, "codeA"},
		{activateHeight - 1, "codeA"}, // last block before activation
		{activateHeight, "codeB"},     // activates exactly at activation height
		{activateHeight + 50, "codeB"},
	}
	for _, tc := range cases {
		got, err := c.ContractById(id, tc.height)
		assert.NoError(t, err, "height %d", tc.height)
		assert.Equal(t, tc.wantCode, got.Code, "active code at height %d", tc.height)
	}
}

// FindPendingUpdates lists the queued update (code, proposer, activation) while it
// is pending, and nothing once it has activated.
func TestFindPendingUpdates(t *testing.T) {
	c, hb, _, _ := setupContractsDB(t)
	seedBlocks(t, hb, createHeight, updateHeight)
	id := registerWithTimelock(c)

	pending, err := c.FindPendingUpdates(nil, updateHeight, 0, 100)
	assert.NoError(t, err)
	assert.Len(t, pending, 1, "one update should be pending mid-window")
	assert.Equal(t, "codeB", pending[0].Code)
	assert.Equal(t, "hive:alice", pending[0].Proposer)
	assert.Equal(t, activateHeight, pending[0].ActivationHeight)
	assert.Equal(t, id, pending[0].Id)

	activated, err := c.FindPendingUpdates(nil, activateHeight, 0, 100)
	assert.NoError(t, err)
	assert.Len(t, activated, 0, "nothing pending once activated")
}

// FindActiveContracts returns the active version per id (old code while a newer
// version is still queued, new code after it activates) and never a pending one.
func TestFindActiveContracts(t *testing.T) {
	c, hb, _, _ := setupContractsDB(t)
	seedBlocks(t, hb, createHeight, updateHeight)
	registerWithTimelock(c)

	mid, err := c.FindActiveContracts(nil, nil, updateHeight, 0, 100)
	assert.NoError(t, err)
	assert.Len(t, mid, 1)
	assert.Equal(t, "codeA", mid[0].Code, "active is still old code during window")

	after, err := c.FindActiveContracts(nil, nil, activateHeight, 0, 100)
	assert.NoError(t, err)
	assert.Len(t, after, 1)
	assert.Equal(t, "codeB", after[0].Code, "active is new code after activation")
}

// A cancelled update is tombstoned and never activates; the contract keeps running
// the previously-active code even past the original activation height.
func TestCancelPendingUpdate(t *testing.T) {
	c, hb, _, _ := setupContractsDB(t)
	seedBlocks(t, hb, createHeight, updateHeight)
	id := registerWithTimelock(c)

	n, err := c.CancelPendingUpdate(id, updateHeight, updateHeight, "tx-cancel", nil)
	assert.NoError(t, err)
	assert.Equal(t, 1, n, "one pending version cancelled")

	// Past the original activation height the old code still runs.
	got, err := c.ContractById(id, activateHeight+10)
	assert.NoError(t, err)
	assert.Equal(t, "codeA", got.Code, "cancelled update must not activate")

	pending, err := c.FindPendingUpdates(nil, updateHeight, 0, 100)
	assert.NoError(t, err)
	assert.Len(t, pending, 0, "cancelled update no longer pending")

	// Cancelling again cancels nothing.
	n, err = c.CancelPendingUpdate(id, updateHeight, updateHeight, "tx-cancel2", nil)
	assert.NoError(t, err)
	assert.Equal(t, 0, n)
}

// RegisterContract must never let a version resolve before it was created: an
// unset ActivationHeight defaults to CreationHeight (immediate), so a height-
// scoped lookup below the creation height returns nothing.
func TestRegisterContract_DefaultActivationGuard(t *testing.T) {
	c, _, _, _ := setupContractsDB(t)
	id := "vsc1defaultguard"
	c.RegisterContract(id, contracts.Contract{
		Code:           "codeX",
		Creator:        "hive:bob",
		Owner:          "hive:bob",
		TxId:           "tx-x",
		CreationHeight: 120,
		// ActivationHeight intentionally left zero.
		Runtime: wasm_runtime.Go,
	})

	_, err := c.ContractById(id, 119)
	assert.Error(t, err, "version must not be visible before its creation height")

	got, err := c.ContractById(id, 120)
	assert.NoError(t, err)
	assert.Equal(t, "codeX", got.Code)
	assert.Equal(t, uint64(120), got.ActivationHeight, "activation defaults to creation height")
}

// Init backfills activation_height = creation_height for rows written before the
// timelock feature, so the activation-aware ContractById reproduces pre-feature
// results exactly.
func TestInitBackfillsActivationHeight(t *testing.T) {
	c, _, dbi, dbConfig := setupContractsDB(t)

	// Insert a legacy-shaped doc with NO activation_height field.
	coll := dbi.Database(dbConfig.GetDbName()).Collection("contracts")
	_, err := coll.InsertOne(context.Background(), bson.M{
		"id":              "vsc1legacy",
		"code":            "legacyCode",
		"creator":         "hive:carol",
		"owner":           "hive:carol",
		"tx_id":           "tx-legacy",
		"creation_height": uint64(50),
		"latest":          true,
	})
	assert.NoError(t, err)

	// Re-run Init to trigger the backfill.
	assert.NoError(t, c.Init())

	// Before backfill the activation filter would have skipped it; after backfill
	// (activation = creation = 50) it is the active version at height >= 50.
	got, err := c.ContractById("vsc1legacy", 50)
	assert.NoError(t, err)
	assert.Equal(t, "legacyCode", got.Code)
	assert.Equal(t, uint64(50), got.ActivationHeight)

	_, err = c.ContractById("vsc1legacy", 49)
	assert.Error(t, err, "legacy row must not resolve below its creation height after backfill")
}
