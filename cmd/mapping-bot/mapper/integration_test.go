package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"testing"
	"time"
	"vsc-node/cmd/mapping-bot/chain"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// testBotConfig is a minimal BotConfiger for mapper-package tests.
type testBotConfig struct{}

func (c *testBotConfig) ContractId() string { return "test-contract" }
func (c *testBotConfig) HttpPort() uint16   { return 0 }
func (c *testBotConfig) SignApiKey() string  { return "" }
func (c *testBotConfig) FilePath() string    { return "" }

// newTestBotWithMocks creates a Bot wired to all mock dependencies.
// Returns the bot and all mocks so the caller can inspect/configure them.
func newTestBotWithMocks() (
	bot *Bot,
	gql *mockGraphQL,
	caller *mockContractCaller,
	state *mockStateStore,
	addr *mockAddressStore,
	chainClient *mockChainClient,
) {
	gql = &mockGraphQL{}
	caller = &mockContractCaller{}
	state = newMockStateStore()
	addr = newMockAddressStore()
	chainClient = newMockChainClient()

	bot = &Bot{
		L:           slog.Default(),
		BotConfig:   &testBotConfig{},
		ChainParams: &chaincfg.TestNet4Params,
		Chain: &chain.ChainConfig{
			Name:        "btc",
			AssetSymbol: "BTC",
			Client:      chainClient,
			ChainParams: &chaincfg.TestNet4Params,
		},
		Gql:     gql,
		Caller:  caller,
		StateDB: state,
		AddrDB:  addr,
	}
	return
}

// buildTestBlock creates a minimal Bitcoin block containing one coinbase tx
// and one payment tx that sends to the given address. Returns serialized block bytes.
func buildTestBlock(t *testing.T, destAddr string, params *chaincfg.Params) []byte {
	t.Helper()

	// Coinbase transaction
	coinbaseTx := wire.NewMsgTx(wire.TxVersion)
	coinbaseTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{}, Index: 0xffffffff},
		SignatureScript:  []byte{0x04, 0xff, 0xff, 0x00, 0x1d, 0x01, 0x04},
		Sequence:         0xffffffff,
	})
	coinbaseTx.AddTxOut(&wire.TxOut{Value: 50e8, PkScript: []byte{txscript.OP_TRUE}})

	// Payment transaction to our deposit address
	paymentTx := wire.NewMsgTx(wire.TxVersion)
	// Dummy input (not validated in our tests)
	prevHash := coinbaseTx.TxHash()
	paymentTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: prevHash, Index: 0},
		SignatureScript:  []byte{txscript.OP_TRUE},
		Sequence:         0xffffffff,
	})

	// Create the output script for the destination address
	addr, err := btcutil.DecodeAddress(destAddr, params)
	require.NoError(t, err)
	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	paymentTx.AddTxOut(&wire.TxOut{Value: 10000, PkScript: pkScript})

	// Build the block
	block := wire.MsgBlock{
		Header: wire.BlockHeader{
			Version:    1,
			PrevBlock:  chainhash.Hash{},
			MerkleRoot: chainhash.Hash{}, // will be filled below
			Timestamp:  time.Now(),
			Bits:       0x1d00ffff,
			Nonce:      0,
		},
		Transactions: []*wire.MsgTx{coinbaseTx, paymentTx},
	}

	// Compute merkle root
	txHashes := make([]*chainhash.Hash, len(block.Transactions))
	for i, tx := range block.Transactions {
		h := tx.TxHash()
		txHashes[i] = &h
	}
	merkleRoot := chainhash.DoubleHashH(append(txHashes[0][:], txHashes[1][:]...))
	block.Header.MerkleRoot = merkleRoot

	var buf bytes.Buffer
	require.NoError(t, block.Serialize(&buf))
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// TestHandleMap_EndToEnd
// ---------------------------------------------------------------------------

func TestHandleMap_EndToEnd(t *testing.T) {
	bot, gql, caller, state, addr, _ := newTestBotWithMocks()

	// Set contract's last height high enough that block 100 is already known
	gql.lastHeight = "200"

	// Register a known deposit address so ParseBlock can find it.
	// We need a real P2WSH address on testnet4.
	depositAddr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[depositAddr] = "deposit_to=hive:testuser"

	// Build a block with a tx sending to that address
	blockBytes := buildTestBlock(t, depositAddr, &chaincfg.TestNet4Params)

	bot.HandleMap(blockBytes, 100)

	// Verify: contract caller should have received a "map" call
	calls := caller.getCalls()
	require.NotEmpty(t, calls, "expected at least one contract call")
	assert.Equal(t, "map", calls[0].Action)

	// Verify: block height was incremented
	h, err := state.GetBlockHeight(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), h)
}

// ---------------------------------------------------------------------------
// TestHandleMap_BlockNotYetInContract
// ---------------------------------------------------------------------------

func TestHandleMap_BlockNotYetInContract(t *testing.T) {
	bot, gql, caller, state, _, _ := newTestBotWithMocks()

	// Contract's last height is lower than the block we're processing
	gql.lastHeight = "50"

	bot.HandleMap([]byte{}, 100)

	// Should not call the contract or increment height
	assert.Empty(t, caller.getCalls())
	h, _ := state.GetBlockHeight(context.Background())
	assert.Equal(t, uint64(0), h)
}

// ---------------------------------------------------------------------------
// TestHandleUnmap_EndToEnd
// ---------------------------------------------------------------------------

func TestHandleUnmap_EndToEnd(t *testing.T) {
	bot, gql, _, state, _, _ := newTestBotWithMocks()

	sigHash := make([]byte, 32)
	sigHash[0] = 0xAA

	// GraphQL returns a new tx spend
	gql.txSpends = map[string]*contractinterface.SigningData{
		"txUnmap1": {
			Tx: []byte{0x01, 0x02},
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{
				{Index: 0, SigHash: sigHash, WitnessScript: []byte{0xDE, 0xAD}},
			},
		},
	}

	// No signatures available yet
	gql.signatures = make(map[string]database.SignatureUpdate)

	bot.HandleUnmap()

	// Verify: tx was stored as pending
	tx, err := state.GetPendingTransaction(context.Background(), "txUnmap1")
	require.NoError(t, err)
	assert.Equal(t, database.TxStatePending, tx.State)
}

// ---------------------------------------------------------------------------
// TestHandleConfirmations_EndToEnd
// ---------------------------------------------------------------------------

func TestHandleConfirmations_EndToEnd(t *testing.T) {
	bot, _, caller, state, _, chainClient := newTestBotWithMocks()

	// Add a sent tx to the state store
	sigHash := make([]byte, 32)
	sigHash[0] = 0xBB
	require.NoError(t, state.AddPendingTransaction(
		context.Background(), "txConfirm1", []byte{0x01},
		[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}},
	))
	require.NoError(t, state.MarkTransactionSent(context.Background(), "txConfirm1"))

	// Chain reports the tx as confirmed
	chainClient.txStatuses["txConfirm1"] = true

	bot.HandleConfirmations()

	// Verify: contract caller received a "confirmSpend" call
	calls := caller.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "confirmSpend", calls[0].Action)
	assert.Contains(t, string(calls[0].Payload), "txConfirm1")

	// Verify: tx is now confirmed in the state store
	state.mu.Lock()
	tx := state.txs["txConfirm1"]
	state.mu.Unlock()
	require.NotNil(t, tx)
	assert.Equal(t, database.TxStateConfirmed, tx.State)
}

// ---------------------------------------------------------------------------
// TestHandleConfirmations_NotYetConfirmed
// ---------------------------------------------------------------------------

func TestHandleConfirmations_NotYetConfirmed(t *testing.T) {
	bot, _, caller, state, _, chainClient := newTestBotWithMocks()

	sigHash := make([]byte, 32)
	sigHash[0] = 0xCC
	require.NoError(t, state.AddPendingTransaction(
		context.Background(), "txNotConfirmed", []byte{0x01},
		[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}},
	))
	require.NoError(t, state.MarkTransactionSent(context.Background(), "txNotConfirmed"))

	// Chain reports the tx as NOT confirmed
	chainClient.txStatuses["txNotConfirmed"] = false

	bot.HandleConfirmations()

	// Should not call the contract
	assert.Empty(t, caller.getCalls())

	// tx should still be in "sent" state
	state.mu.Lock()
	tx := state.txs["txNotConfirmed"]
	state.mu.Unlock()
	assert.Equal(t, database.TxStateSent, tx.State)
}

// ---------------------------------------------------------------------------
// TestProcessTxSpends_NewTransaction
// ---------------------------------------------------------------------------

func TestProcessTxSpends_NewTransaction(t *testing.T) {
	bot, _, _, state, _, _ := newTestBotWithMocks()

	sigHash := make([]byte, 32)
	sigHash[0] = 0x11
	spends := map[string]*contractinterface.SigningData{
		"txNew": {
			Tx: []byte{0x01},
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{
				{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}},
			},
		},
	}

	bot.ProcessTxSpends(context.Background(), spends)

	tx, err := state.GetPendingTransaction(context.Background(), "txNew")
	require.NoError(t, err)
	assert.Equal(t, database.TxStatePending, tx.State)
	assert.Equal(t, uint64(1), tx.TotalSignatures)
}

// ---------------------------------------------------------------------------
// TestProcessTxSpends_AlreadySent
// ---------------------------------------------------------------------------

func TestProcessTxSpends_AlreadySent(t *testing.T) {
	bot, _, _, state, _, _ := newTestBotWithMocks()

	sigHash := make([]byte, 32)
	sigHash[0] = 0x22

	// Pre-populate: add and mark as sent
	require.NoError(t, state.AddPendingTransaction(
		context.Background(), "txAlreadySent", []byte{0x01},
		[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}},
	))
	require.NoError(t, state.MarkTransactionSent(context.Background(), "txAlreadySent"))

	spends := map[string]*contractinterface.SigningData{
		"txAlreadySent": {
			Tx: []byte{0x01},
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{
				{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}},
			},
		},
	}

	bot.ProcessTxSpends(context.Background(), spends)

	// Should still be in sent state, not re-added as pending
	state.mu.Lock()
	tx := state.txs["txAlreadySent"]
	state.mu.Unlock()
	assert.Equal(t, database.TxStateSent, tx.State)
}

// ---------------------------------------------------------------------------
// TestCheckSignatures_FullySigned
// ---------------------------------------------------------------------------

func TestCheckSignatures_FullySigned(t *testing.T) {
	bot, gql, _, state, _, _ := newTestBotWithMocks()

	sigHash := make([]byte, 32)
	sigHash[0] = 0xDD
	sigHashHex := hex.EncodeToString(sigHash)

	require.NoError(t, state.AddPendingTransaction(
		context.Background(), "txToSign", []byte{0x01},
		[]contractinterface.UnsignedSigHash{
			{Index: 0, SigHash: sigHash, WitnessScript: []byte{0xDE, 0xAD}},
		},
	))

	// Mock GraphQL returns a completed signature for this sighash.
	// Note: the mockStateStore uses the raw sigHash bytes as the key (string(sigHash)),
	// while the real DB uses hex-encoded strings. We need to match what
	// GetAllPendingSigHashes returns.
	fakeSig := make([]byte, 64)
	fakeSig[0] = 0xFF

	// GetAllPendingSigHashes in the mock returns string(sigHash) — raw bytes.
	// FetchSignatures receives those hashes and returns a map keyed by the same string.
	// So we key the mock signatures by string(sigHash) as well.
	gql.signatures = map[string]database.SignatureUpdate{
		string(sigHash): {Bytes: fakeSig},
	}

	fullySignedTxs, err := bot.CheckSignagures(context.Background())
	require.NoError(t, err)
	require.Len(t, fullySignedTxs, 1)
	assert.Equal(t, "txToSign", fullySignedTxs[0].TxID)

	// Verify FetchSignatures was called
	gql.mu.Lock()
	var fetchSigCalls int
	for _, c := range gql.calls {
		if c.Method == "FetchSignatures" {
			fetchSigCalls++
		}
	}
	gql.mu.Unlock()
	assert.Equal(t, 1, fetchSigCalls)
	_ = sigHashHex // used for documentation, the mock keys on raw bytes
}

// ---------------------------------------------------------------------------
// TestCheckSignatures_NoSignaturesAvailable
// ---------------------------------------------------------------------------

func TestCheckSignatures_NoSignaturesAvailable(t *testing.T) {
	bot, gql, _, state, _, _ := newTestBotWithMocks()

	sigHash := make([]byte, 32)
	sigHash[0] = 0xEE
	require.NoError(t, state.AddPendingTransaction(
		context.Background(), "txUnsigned", []byte{0x01},
		[]contractinterface.UnsignedSigHash{
			{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}},
		},
	))

	// No signatures available
	gql.signatures = make(map[string]database.SignatureUpdate)

	fullySignedTxs, err := bot.CheckSignagures(context.Background())
	require.NoError(t, err)
	assert.Empty(t, fullySignedTxs)

	// tx should still be pending
	tx, err := state.GetPendingTransaction(context.Background(), "txUnsigned")
	require.NoError(t, err)
	assert.Equal(t, database.TxStatePending, tx.State)
}

// ---------------------------------------------------------------------------
// TestProcessTxSpends_MultipleMixed
// ---------------------------------------------------------------------------

func TestProcessTxSpends_MultipleMixed(t *testing.T) {
	bot, _, _, state, _, _ := newTestBotWithMocks()

	// Pre-populate one sent tx
	sigHash1 := make([]byte, 32)
	sigHash1[0] = 0x44
	require.NoError(t, state.AddPendingTransaction(
		context.Background(), "txSent", []byte{0x01},
		[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash1, WitnessScript: []byte{0x01}}},
	))
	require.NoError(t, state.MarkTransactionSent(context.Background(), "txSent"))

	// Pre-populate one already pending
	sigHash2 := make([]byte, 32)
	sigHash2[0] = 0x55
	require.NoError(t, state.AddPendingTransaction(
		context.Background(), "txPending", []byte{0x02},
		[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash2, WitnessScript: []byte{0x02}}},
	))

	// Incoming spends: one sent (skip), one pending (skip), one new (add)
	sigHash3 := make([]byte, 32)
	sigHash3[0] = 0x66
	spends := map[string]*contractinterface.SigningData{
		"txSent": {
			Tx:                []byte{0x01},
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash1, WitnessScript: []byte{0x01}}},
		},
		"txPending": {
			Tx:                []byte{0x02},
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash2, WitnessScript: []byte{0x02}}},
		},
		"txBrandNew": {
			Tx:                []byte{0x03},
			UnsignedSigHashes: []contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash3, WitnessScript: []byte{0x03}}},
		},
	}

	bot.ProcessTxSpends(context.Background(), spends)

	// txSent should still be sent
	state.mu.Lock()
	assert.Equal(t, database.TxStateSent, state.txs["txSent"].State)
	// txPending should still be pending (not duplicated)
	assert.Equal(t, database.TxStatePending, state.txs["txPending"].State)
	// txBrandNew should be newly added as pending
	assert.Equal(t, database.TxStatePending, state.txs["txBrandNew"].State)
	assert.Equal(t, 3, len(state.txs))
	state.mu.Unlock()
}

// ---------------------------------------------------------------------------
// TestHandleConfirmations_MultipleTransactions
// ---------------------------------------------------------------------------

func TestHandleConfirmations_MultipleTransactions(t *testing.T) {
	bot, _, caller, state, _, chainClient := newTestBotWithMocks()
	ctx := context.Background()

	// Add two sent transactions
	for _, txID := range []string{"txA", "txB"} {
		sigHash := make([]byte, 32)
		sigHash[0] = txID[2] // use last char as distinguisher
		require.NoError(t, state.AddPendingTransaction(ctx, txID, []byte{0x01},
			[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: sigHash, WitnessScript: []byte{0x01}}},
		))
		require.NoError(t, state.MarkTransactionSent(ctx, txID))
	}

	// Only txA is confirmed
	chainClient.txStatuses["txA"] = true
	chainClient.txStatuses["txB"] = false

	bot.HandleConfirmations()

	// Only one confirmSpend call should have been made
	calls := caller.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "confirmSpend", calls[0].Action)
	assert.Contains(t, string(calls[0].Payload), "txA")

	// txA should be confirmed, txB still sent
	state.mu.Lock()
	assert.Equal(t, database.TxStateConfirmed, state.txs["txA"].State)
	assert.Equal(t, database.TxStateSent, state.txs["txB"].State)
	state.mu.Unlock()
}
