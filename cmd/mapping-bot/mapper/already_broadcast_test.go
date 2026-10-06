package mapper

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"vsc-node/cmd/mapping-bot/chain"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The exact error mempool.space returned on testnet 2026-10-06 for a spend that was
// mined long before the bot tried to post it.
const minedErr = `API returned status 400: sendrawtransaction RPC error: {"code":-27,"message":"Transaction outputs already in utxo set"}`

func TestIsAlreadyBroadcast(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New(minedErr), true},
		{errors.New(`sendrawtransaction RPC error: {"code":-27,"message":"Transaction already in block chain"}`), true},
		{errors.New(`sendrawtransaction RPC error: {"code":-26,"message":"txn-already-in-mempool"}`), true},
		{errors.New(`sendrawtransaction RPC error: {"code":-27,"message":"txn-already-known"}`), true},
		{errors.New(`sendrawtransaction RPC error: {"code":-26,"message":"min relay fee not met"}`), false},
		{errors.New(`sendrawtransaction RPC error: {"code":-25,"message":"bad-txns-inputs-missingorspent"}`), false},
		{nil, false},
	} {
		if got := isAlreadyBroadcast(tc.err); got != tc.want {
			t.Errorf("isAlreadyBroadcast(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// oneInputTx is a minimal transaction attachSignatures can work on, and its txid
// (the bot keys spends by their real txid; witnesses do not change it).
func oneInputTx(t *testing.T, seed byte) ([]byte, string) {
	t.Helper()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{seed}, Index: 0}, Sequence: 0xfffffffd})
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: []byte{0x00, 0x14, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}})
	var buf bytes.Buffer
	require.NoError(t, tx.Serialize(&buf))
	return buf.Bytes(), tx.TxHash().String()
}

// BOT-BCAST-1: a fully signed spend that is already on chain must be marked sent
// after ONE post (no retries, no endless re-posting), so confirmSpend can follow.
func TestHandleUnmap_AlreadyMinedSpendIsMarkedSent(t *testing.T) {
	bot, gql, _, state, _, chainClient := newTestBotWithMocks()
	sigHash := make([]byte, 32)
	sigHash[0] = 0xA1
	raw, txid := oneInputTx(t, 1)
	gql.txSpends = map[string]*contractinterface.SigningData{
		txid: {
			Tx:                raw,
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0xDE, 0xAD}}},
		},
	}
	// mockStateStore matches signatures by the raw sighash bytes.
	gql.signatures = map[string]database.SignatureUpdate{string(sigHash): {Bytes: []byte{0x30, 0x02, 0x01, 0x01}}}
	chainClient.postTxErr = errors.New(minedErr)

	bot.HandleUnmap()

	state.mu.Lock()
	tx := state.txs[txid]
	state.mu.Unlock()
	require.NotNil(t, tx)
	assert.Equal(t, database.TxStateSent, tx.State, "an already-mined spend must move on to confirmation")
	chainClient.mu.Lock()
	posts := len(chainClient.posted)
	chainClient.mu.Unlock()
	assert.Equal(t, 1, posts, "no retries for a tx the network already has")
}

// Any other broadcast error is still a failure, retried and left unsent.
func TestHandleUnmap_RealBroadcastErrorStaysPending(t *testing.T) {
	bot, gql, _, state, _, chainClient := newTestBotWithMocks()
	sigHash := make([]byte, 32)
	sigHash[0] = 0xA2
	raw, txid := oneInputTx(t, 2)
	gql.txSpends = map[string]*contractinterface.SigningData{
		txid: {
			Tx:                raw,
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0xDE, 0xAD}}},
		},
	}
	gql.signatures = map[string]database.SignatureUpdate{string(sigHash): {Bytes: []byte{0x30, 0x02, 0x01, 0x01}}}
	chainClient.postTxErr = errors.New(`sendrawtransaction RPC error: {"code":-26,"message":"min relay fee not met"}`)

	bot.HandleUnmap()

	state.mu.Lock()
	tx := state.txs[txid]
	state.mu.Unlock()
	require.NotNil(t, tx)
	assert.Equal(t, database.TxStatePending, tx.State)
	chainClient.mu.Lock()
	posts := len(chainClient.posted)
	chainClient.mu.Unlock()
	assert.Equal(t, 3, posts, "a real failure keeps its retries")
}

func sentTxConfirmedAt(t *testing.T, state *mockStateStore, chainClient *mockChainClient, id string, height uint64) {
	t.Helper()
	sigHash := make([]byte, 32)
	sigHash[0] = 0xB1
	require.NoError(t, state.AddPendingTransaction(context.Background(), id, []byte{0x01},
		[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}}))
	require.NoError(t, state.MarkTransactionSent(context.Background(), id, 100))
	blockBytes := buildMinimalBlock(t)
	var block wire.MsgBlock
	require.NoError(t, block.Deserialize(bytes.NewReader(blockBytes)))
	h := block.BlockHash().String()
	chainClient.rawBlocks[h] = blockBytes
	chainClient.txDetails[id] = chain.TxConfirmationDetails{Confirmed: true, BlockHeight: height, BlockHash: h, TxIndex: 0}
}

// A spend mined below the contract's prune floor cannot be proven: skip it rather
// than fail confirmSpend (and pay RC) every cycle.
func TestHandleConfirmations_SkipsSpendBelowPruneFloor(t *testing.T) {
	bot, gql, caller, state, _, chainClient := newTestBotWithMocks()
	gql.lastHeight = "1000"
	gql.pruneFloor = "600"
	gql.txSpends = map[string]*contractinterface.SigningData{"txPruned": {}}
	sentTxConfirmedAt(t, state, chainClient, "txPruned", 500)

	bot.HandleConfirmations()

	assert.Empty(t, caller.getCalls(), "no confirmSpend for a spend whose header is gone")
	state.mu.Lock()
	assert.Equal(t, database.TxStateSent, state.txs["txPruned"].State)
	state.mu.Unlock()
}

// At the floor the header is still there: confirmSpend must go ahead.
func TestHandleConfirmations_ProvesSpendAtThePruneFloor(t *testing.T) {
	bot, gql, caller, state, _, chainClient := newTestBotWithMocks()
	gql.lastHeight = "1000"
	gql.pruneFloor = "500"
	gql.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}
	gql.txSpends = map[string]*contractinterface.SigningData{"txAtFloor": {}}
	sentTxConfirmedAt(t, state, chainClient, "txAtFloor", 500)

	bot.HandleConfirmations()

	calls := caller.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "confirmSpend", calls[0].Action)
}

// BOT-CONF-2 (testnet 2026-10-06): our node restarted during a pass, all three gate
// reads failed, the pass went ahead without them and sent confirmSpend for spends
// mined below the prune floor; each was refused on chain at RC cost and the bot was
// then short of the RC migrateVault needs. One unreadable gate skips the pass.
func TestHandleConfirmations_UnreadableGateSkipsThePass(t *testing.T) {
	for _, method := range []string{"FetchLastHeight", "FetchPruneFloor", "FetchTxSpends"} {
		t.Run(method, func(t *testing.T) {
			bot, gql, caller, state, _, chainClient := newTestBotWithMocks()
			gql.lastHeight = "1000"
			gql.pruneFloor = "600"
			gql.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}
			gql.txSpends = map[string]*contractinterface.SigningData{"txPruned": {}, "txProvable": {}}
			sentTxConfirmedAt(t, state, chainClient, "txPruned", 500)
			sentTxConfirmedAt(t, state, chainClient, "txProvable", 700)
			gql.readErr = map[string]error{method: errors.New("dial tcp 127.0.0.1:8091: connect: connection refused")}

			bot.HandleConfirmations()
			assert.Empty(t, caller.getCalls(), "no confirmSpend while %s is unreadable", method)
			state.mu.Lock()
			assert.Equal(t, database.TxStateSent, state.txs["txPruned"].State)
			assert.Equal(t, database.TxStateSent, state.txs["txProvable"].State, "nothing is marked settled from an unread list")
			state.mu.Unlock()

			// The node answers again: the provable spend is confirmed, the pruned one is not.
			gql.readErr = nil
			bot.HandleConfirmations()
			calls := caller.getCalls()
			require.Len(t, calls, 1)
			assert.Equal(t, "confirmSpend", calls[0].Action)
			state.mu.Lock()
			assert.Equal(t, database.TxStateConfirmed, state.txs["txProvable"].State)
			assert.Equal(t, database.TxStateSent, state.txs["txPruned"].State)
			state.mu.Unlock()
		})
	}
}

// A spend Bitcoin keeps refusing (the testnet phantom legacy coin: an input that
// does not exist) is re-posted at most once per postRetryInterval, not three times
// every cycle until it is abandoned days later.
func TestHandleUnmap_RefusedSpendIsNotRepostedEveryCycle(t *testing.T) {
	bot, gql, _, state, _, chainClient := newTestBotWithMocks()
	sigHash := make([]byte, 32)
	sigHash[0] = 0xA3
	raw, txid := oneInputTx(t, 3)
	gql.txSpends = map[string]*contractinterface.SigningData{
		txid: {
			Tx:                raw,
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0xDE, 0xAD}}},
		},
	}
	gql.signatures = map[string]database.SignatureUpdate{string(sigHash): {Bytes: []byte{0x30, 0x02, 0x01, 0x01}}}
	chainClient.postTxErr = errors.New(`sendrawtransaction RPC error: {"code":-25,"message":"bad-txns-inputs-missingorspent"}`)
	now := time.Now()
	postNow = func() time.Time { return now }
	t.Cleanup(func() { postNow = time.Now })
	posts := func() int { chainClient.mu.Lock(); defer chainClient.mu.Unlock(); return len(chainClient.posted) }

	bot.HandleUnmap()
	require.Equal(t, 3, posts(), "the first cycle keeps its retries")
	bot.HandleUnmap()
	assert.Equal(t, 3, posts(), "the next cycle must not post it again")

	now = now.Add(postRetryInterval)
	bot.HandleUnmap()
	assert.Equal(t, 6, posts(), "after the interval it is tried again")
	state.mu.Lock()
	assert.Equal(t, database.TxStatePending, state.txs[txid].State)
	state.mu.Unlock()
}
