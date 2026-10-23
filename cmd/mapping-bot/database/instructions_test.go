package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// BOT-ADDR-2: the instruction store is the durable record of what the bot watches.

func TestInstructionStore_TouchRenewsAndReportsNew(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	created, err := db.Instructions.Touch(ctx, "deposit_to=hive:alice")
	require.NoError(t, err)
	assert.True(t, created)
	created, err = db.Instructions.Touch(ctx, "deposit_to=hive:alice")
	require.NoError(t, err)
	assert.False(t, created, "a repeat request renews, it does not create")

	all, err := db.Instructions.ListSince(ctx, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"deposit_to=hive:alice"}, all)
	later, err := db.Instructions.ListSince(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, later)
}

// Only an instruction that never received a deposit and was not requested within the
// retention is pruned.
func TestInstructionStore_DeleteStaleKeepsUsedInstructions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	_, err := db.Instructions.Touch(ctx, "deposit_to=hive:unused")
	require.NoError(t, err)
	_, err = db.Instructions.Touch(ctx, "deposit_to=hive:used")
	require.NoError(t, err)
	require.NoError(t, db.Instructions.MarkDeposit(ctx, "deposit_to=hive:used"))

	n, err := db.Instructions.DeleteStale(ctx, time.Hour)
	require.NoError(t, err)
	assert.Zero(t, n, "both were requested within the retention")

	n, err = db.Instructions.DeleteStale(ctx, -time.Minute) // cutoff in the future: everything is "old"
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	left, err := db.Instructions.ListSince(ctx, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"deposit_to=hive:used"}, left, "an instruction with a deposit is never pruned")
}

// An upgraded bot folds the legacy address mappings into the store, idempotently,
// and never moves a newer request time backwards.
func TestInstructionStore_BackfillFromAddresses(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.Addresses.Insert(ctx, "tb1q-gen0-alice", "deposit_to=hive:alice"))
	require.NoError(t, db.Addresses.Insert(ctx, "tb1q-gen1-alice", "deposit_to=hive:alice"))
	require.NoError(t, db.Addresses.Insert(ctx, "tb1q-gen1-bob", "deposit_to=hive:bob"))

	n, err := db.Instructions.BackfillFromAddresses(ctx, db.Addresses)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	all, err := db.Instructions.ListSince(ctx, time.Time{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"deposit_to=hive:alice", "deposit_to=hive:bob"}, all)

	before := time.Now().UTC()
	_, err = db.Instructions.Touch(ctx, "deposit_to=hive:bob")
	require.NoError(t, err)
	_, err = db.Instructions.BackfillFromAddresses(ctx, db.Addresses)
	require.NoError(t, err)
	recent, err := db.Instructions.ListSince(ctx, before)
	require.NoError(t, err)
	assert.Equal(t, []string{"deposit_to=hive:bob"}, recent, "backfill keeps the newer request time")
	all, err = db.Instructions.ListSince(ctx, time.Time{})
	require.NoError(t, err)
	assert.Len(t, all, 2, "idempotent")
}
