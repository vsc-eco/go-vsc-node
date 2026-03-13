package main

import (
	"testing"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProcessTxSpends_NewTransaction verifies that an unseen tx is added as pending.
func TestProcessTxSpends_NewTransaction(t *testing.T) {
	db := setupTestDatabase(t)
	bot := newTestBot(t, db)

	sigHash := make([]byte, 32)
	sigHash[0] = 0x11
	spends := map[string]*contractinterface.SigningData{
		"txNew": {
			Tx:                []byte{0x01},
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}},
		},
	}

	bot.ProcessTxSpends(t.Context(), nil, spends)

	tx, err := db.State.GetPendingTransaction(t.Context(), "txNew")
	require.NoError(t, err)
	assert.Equal(t, database.TxStatePending, tx.State)
}

// TestProcessTxSpends_AlreadySent verifies that a sent tx is not re-added as pending.
func TestProcessTxSpends_AlreadySent(t *testing.T) {
	db := setupTestDatabase(t)
	bot := newTestBot(t, db)

	sigHash := make([]byte, 32)
	sigHash[0] = 0x22

	// Add and immediately mark as sent
	require.NoError(t, db.State.AddPendingTransaction(t.Context(), "txAlreadySent", []byte{0x01},
		[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}},
	))
	require.NoError(t, db.State.MarkTransactionSent(t.Context(), "txAlreadySent"))

	spends := map[string]*contractinterface.SigningData{
		"txAlreadySent": {
			Tx:                []byte{0x01},
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}},
		},
	}

	bot.ProcessTxSpends(t.Context(), nil, spends)

	// Should still not be pending (was sent)
	_, err := db.State.GetPendingTransaction(t.Context(), "txAlreadySent")
	assert.Error(t, err)
}

// TestProcessTxSpends_AlreadyPending verifies that a pending tx is not re-added.
func TestProcessTxSpends_AlreadyPending(t *testing.T) {
	db := setupTestDatabase(t)
	bot := newTestBot(t, db)

	sigHash := make([]byte, 32)
	sigHash[0] = 0x33

	require.NoError(t, db.State.AddPendingTransaction(t.Context(), "txPending", []byte{0x01},
		[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}},
	))

	spends := map[string]*contractinterface.SigningData{
		"txPending": {
			Tx:                []byte{0x01},
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}},
		},
	}

	// Should not panic or create a duplicate
	bot.ProcessTxSpends(t.Context(), nil, spends)

	txs, err := db.State.GetAllPendingTransactions(t.Context())
	require.NoError(t, err)
	assert.Len(t, txs, 1, "should still only have the original pending tx")
}

// TestNoopMempoolClient_DoesNotBroadcast verifies the stub never calls the real network.
func TestNoopMempoolClient_DoesNotBroadcast(t *testing.T) {
	noop := &noopMempoolClient{}
	require.NoError(t, noop.PostTx("deadbeef"))
	assert.Equal(t, []string{"deadbeef"}, noop.posted, "noop should record but not broadcast")
}

