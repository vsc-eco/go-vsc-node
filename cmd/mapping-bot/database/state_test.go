package database_test

import (
	"context"
	"encoding/hex"
	"testing"
	"time"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "mappingbottest_state"
const testMongoURI = "mongodb://localhost:27017"

func setupTestDB(t *testing.T) *database.Database {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(ctx, testMongoURI, testDBName)
	if err != nil {
		t.Skipf("MongoDB not available: %s", err)
	}
	t.Cleanup(func() {
		if err := db.DropDatabase(context.Background()); err != nil {
			t.Logf("failed to drop test database: %s", err)
		}
		db.Close(context.Background())
	})
	return db
}

func makeSigHash(b byte) []byte {
	h := make([]byte, 32)
	h[0] = b
	return h
}

func makeUnsignedSigHashes(sigHashes ...[]byte) []contractinterface.UnsignedSigHash {
	slots := make([]contractinterface.UnsignedSigHash, len(sigHashes))
	for i, h := range sigHashes {
		slots[i] = contractinterface.UnsignedSigHash{
			Index:         uint32(i),
			SigHash:       h,
			WitnessScript: []byte{0xde, 0xad, 0xbe, 0xef},
		}
	}
	return slots
}

func TestAddPendingTransaction(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	err := db.State.AddPendingTransaction(ctx, "txA", []byte{0x01}, makeUnsignedSigHashes(makeSigHash(1)))
	require.NoError(t, err)

	tx, err := db.State.GetPendingTransaction(ctx, "txA")
	require.NoError(t, err)
	assert.Equal(t, database.TxStatePending, tx.State)
	assert.Equal(t, uint64(1), tx.TotalSignatures)
	assert.Equal(t, uint64(0), tx.CurrentSignatures)
	assert.Len(t, tx.Signatures, 1)
}

func TestAddPendingTransaction_Duplicate(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.State.AddPendingTransaction(ctx, "txDup", []byte{0x01}, makeUnsignedSigHashes(makeSigHash(1))))
	err := db.State.AddPendingTransaction(ctx, "txDup", []byte{0x01}, makeUnsignedSigHashes(makeSigHash(1)))
	assert.ErrorIs(t, err, database.ErrTxExists)
}

func TestMarkTransactionSent(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.State.AddPendingTransaction(ctx, "txSent", []byte{0x02}, makeUnsignedSigHashes(makeSigHash(2))))
	require.NoError(t, db.State.MarkTransactionSent(ctx, "txSent"))

	// Should no longer appear as pending
	_, err := db.State.GetPendingTransaction(ctx, "txSent")
	assert.Error(t, err)

	// Should appear as processed
	processed, err := db.State.IsTransactionProcessed(ctx, "txSent")
	require.NoError(t, err)
	assert.True(t, processed)
}

func TestMarkTransactionSent_NotFound(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	err := db.State.MarkTransactionSent(ctx, "doesNotExist")
	assert.ErrorIs(t, err, database.ErrTxNotFound)
}

func TestMarkTransactionConfirmed(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.State.AddPendingTransaction(ctx, "txConfirm", []byte{0x03}, makeUnsignedSigHashes(makeSigHash(3))))
	require.NoError(t, db.State.MarkTransactionSent(ctx, "txConfirm"))
	require.NoError(t, db.State.MarkTransactionConfirmed(ctx, "txConfirm"))

	processed, err := db.State.IsTransactionProcessed(ctx, "txConfirm")
	require.NoError(t, err)
	assert.True(t, processed)
}

func TestMarkTransactionConfirmed_NotSent(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Confirming a pending (not yet sent) tx should fail
	require.NoError(t, db.State.AddPendingTransaction(ctx, "txNotSent", []byte{0x04}, makeUnsignedSigHashes(makeSigHash(4))))
	err := db.State.MarkTransactionConfirmed(ctx, "txNotSent")
	assert.ErrorIs(t, err, database.ErrTxNotFound)
}

func TestIsTransactionProcessed_Pending(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.State.AddPendingTransaction(ctx, "txPend", []byte{0x05}, makeUnsignedSigHashes(makeSigHash(5))))

	processed, err := db.State.IsTransactionProcessed(ctx, "txPend")
	require.NoError(t, err)
	assert.False(t, processed)
}

func TestIsTransactionProcessed_Unknown(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	processed, err := db.State.IsTransactionProcessed(ctx, "unknown")
	require.NoError(t, err)
	assert.False(t, processed)
}

func TestUpdateSignatures_IncrementsCount(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	sigHash := makeSigHash(0xAA)
	require.NoError(t, db.State.AddPendingTransaction(ctx, "txSig", []byte{0x06}, makeUnsignedSigHashes(sigHash)))

	sig := make([]byte, 64)
	sig[0] = 0xFF
	sigMap := map[string][]byte{
		hex.EncodeToString(sigHash): sig,
	}

	fullySigned, err := db.State.UpdateSignatures(ctx, sigMap)
	require.NoError(t, err)
	assert.Len(t, fullySigned, 1, "single-sig tx should be fully signed after one update")

	// Slot should now be filled and currentSignatures incremented
	tx, err := db.State.GetPendingTransaction(ctx, "txSig")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), tx.CurrentSignatures)
	assert.NotNil(t, tx.Signatures[0].Signature)
}

func TestUpdateSignatures_MultiSlot(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	h1, h2 := makeSigHash(0xB1), makeSigHash(0xB2)
	require.NoError(t, db.State.AddPendingTransaction(ctx, "txMulti", []byte{0x07}, makeUnsignedSigHashes(h1, h2)))

	sig := []byte{0x01, 0x02}

	// Fill only first slot — should NOT be fully signed yet
	fullySigned, err := db.State.UpdateSignatures(ctx, map[string][]byte{
		hex.EncodeToString(h1): sig,
	})
	require.NoError(t, err)
	assert.Empty(t, fullySigned)

	// Fill second slot — now fully signed
	fullySigned, err = db.State.UpdateSignatures(ctx, map[string][]byte{
		hex.EncodeToString(h2): sig,
	})
	require.NoError(t, err)
	assert.Len(t, fullySigned, 1)
}

func TestUpdateSignatures_UnknownSigHash(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Updating with a hash that doesn't exist in any pending tx should be a no-op
	fullySigned, err := db.State.UpdateSignatures(ctx, map[string][]byte{
		hex.EncodeToString(makeSigHash(0xFF)): {0x01},
	})
	require.NoError(t, err)
	assert.Empty(t, fullySigned)
}

func TestGetAllPendingSigHashes(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	h1 := makeSigHash(0xC1)
	h2 := makeSigHash(0xC2)
	require.NoError(t, db.State.AddPendingTransaction(ctx, "txHash1", []byte{0x08}, makeUnsignedSigHashes(h1)))
	require.NoError(t, db.State.AddPendingTransaction(ctx, "txHash2", []byte{0x09}, makeUnsignedSigHashes(h2)))

	hashes, err := db.State.GetAllPendingSigHashes(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		hex.EncodeToString(h1),
		hex.EncodeToString(h2),
	}, hashes)
}

func TestGetAllPendingSigHashes_ExcludesSigned(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	h := makeSigHash(0xD1)
	require.NoError(t, db.State.AddPendingTransaction(ctx, "txHashSigned", []byte{0x0A}, makeUnsignedSigHashes(h)))
	_, err := db.State.UpdateSignatures(ctx, map[string][]byte{hex.EncodeToString(h): {0x01}})
	require.NoError(t, err)

	hashes, err := db.State.GetAllPendingSigHashes(ctx)
	require.NoError(t, err)
	assert.NotContains(t, hashes, hex.EncodeToString(h))
}

func TestDeleteOldPendingTransactions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.State.AddPendingTransaction(ctx, "txOld", []byte{0x0B}, makeUnsignedSigHashes(makeSigHash(0xE1))))

	// Negative age sets cutoff in the future, so newly inserted docs are included
	count, err := db.State.DeleteOldPendingTransactions(ctx, -time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}
