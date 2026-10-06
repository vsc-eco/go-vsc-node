package mapper

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/lib/btcvault"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/hasura/go-graphql-client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyNode answers getStateByKeys for the deposit-key read.
func keyNode(t *testing.T, state map[string]string) *Bot {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		out := map[string]interface{}{}
		keys, _ := req.Variables["keys"].([]interface{})
		for _, k := range keys {
			if v, ok := state[k.(string)]; ok {
				out[k.(string)] = v
			} else {
				out[k.(string)] = nil
			}
		}
		b, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"getStateByKeys": out}})
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return &Bot{GqlClient: graphql.NewClient(srv.URL, srv.Client()), BotConfig: l2TestBotConfig{}, L: slog.Default()}
}

func keyedEntry(gen uint32, status btcvault.VaultStatus, primaryByte byte) []byte {
	e := vaultEntry(gen, status)
	for i := 0; i < 33; i++ {
		e[4+i] = primaryByte
	}
	e[4] = 0x02
	return e
}

// BOT-ADDR-1: with gen 0 draining and gen 1 active, new deposit addresses must be
// derived from gen 1's keys, not the legacy pubkey/backupkey (still gen 0's).
func TestFetchPublicKeys_UsesTheActiveGeneration(t *testing.T) {
	va := make([]byte, 4)
	binary.BigEndian.PutUint32(va, 1)
	reg := append(keyedEntry(0, btcvault.VaultStatusDraining, 0xA0), keyedEntry(1, btcvault.VaultStatusActive, 0xB1)...)
	bot := keyNode(t, map[string]string{
		"v": hex.EncodeToString(reg), "va": hex.EncodeToString(va),
		"pubkey": "02" + strings.Repeat("a0", 32), "backupkey": "03" + strings.Repeat("a0", 32),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	primary, _, err := bot.FetchPublicKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, "02"+strings.Repeat("b1", 32), hex.EncodeToString(primary), "deposit key must be the ACTIVE generation's")
}

// A registry whose active pointer names a generation that is not Active hands out
// no address at all, rather than one for a retiring key.
func TestFetchPublicKeys_RefusesANonActiveGeneration(t *testing.T) {
	va := make([]byte, 4)
	binary.BigEndian.PutUint32(va, 0)
	bot := keyNode(t, map[string]string{"v": hex.EncodeToString(keyedEntry(0, btcvault.VaultStatusDraining, 0xA0)), "va": hex.EncodeToString(va)})
	_, _, err := bot.FetchPublicKeys(context.Background())
	assert.Error(t, err)
}

// No registry (pre-fold vault, other chains): the legacy key pair, as before.
func TestFetchPublicKeys_NoRegistryUsesLegacyKeys(t *testing.T) {
	bot := keyNode(t, map[string]string{"pubkey": "02" + strings.Repeat("c3", 32), "backupkey": "03" + strings.Repeat("c3", 32)})
	primary, backup, err := bot.FetchPublicKeys(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "02"+strings.Repeat("c3", 32), hex.EncodeToString(primary))
	assert.Equal(t, "03"+strings.Repeat("c3", 32), hex.EncodeToString(backup))
}

// BOT-MAP-DEPTH-1: with 2 confirmations required, a block the contract holds at
// depth 1 is not mapped yet (and the height does not advance); at depth 2 it is.
func TestHandleMap_WaitsForTheContractDepth(t *testing.T) {
	bot, gql, caller, state, addr, _ := newTestBotWithMocks()
	bot.Chain.ConfirmationsRequired = 2
	gql.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}
	depositAddr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[depositAddr] = "deposit_to=hive:testuser"
	blockBytes := buildTestBlock(t, depositAddr, &chaincfg.TestNet4Params)
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))

	gql.lastHeight = "101" // contract tip 101, block 100: depth 1 (the contract's tip - height)
	assert.False(t, bot.HandleMap(blockBytes, 100))
	assert.Empty(t, caller.getCalls(), "a map at depth 1 is refused by the contract")
	h, _ := state.GetBlockHeight(context.Background())
	assert.Equal(t, uint64(100), h, "the block must not be skipped")

	gql.lastHeight = "102" // depth 2
	assert.True(t, bot.HandleMap(blockBytes, 100))
	calls := caller.getCalls()
	require.NotEmpty(t, calls)
	assert.Equal(t, "map", calls[0].Action)
}

// BOT-CONF-1: confirmSpend waits for the contract depth, and a spend the contract
// no longer lists as pending is recorded confirmed without another call.
func TestHandleConfirmations_WaitsForDepthAndSkipsSettled(t *testing.T) {
	bot, gql, caller, state, _, chainClient := newTestBotWithMocks()
	bot.Chain.ConfirmationsRequired = 2
	gql.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}
	gql.txSpends = map[string]*contractinterface.SigningData{"txDeep": {}}
	sentTxConfirmedAt(t, state, chainClient, "txDeep", 500)
	sentTxConfirmedAt(t, state, chainClient, "txSettled", 500)

	gql.lastHeight = "501" // depth 1: refused on testnet 2026-10-06 ("sits 1 below a tip ..., and 2 is required")
	bot.HandleConfirmations()
	for _, c := range caller.getCalls() {
		t.Errorf("confirmSpend sent at depth 1: %+v", c)
	}
	state.mu.Lock()
	assert.Equal(t, database.TxStateSent, state.txs["txDeep"].State)
	state.mu.Unlock()

	gql.lastHeight = "502" // depth 2
	bot.HandleConfirmations()
	calls := caller.getCalls()
	require.Len(t, calls, 1, "only the still-pending spend is confirmed")
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.Equal(t, database.TxStateConfirmed, state.txs["txDeep"].State)
	assert.Equal(t, database.TxStateConfirmed, state.txs["txSettled"].State, "a spend the contract already settled is recorded confirmed")
}

// BOT-MAP-SKIP-1: a map that did not land for a transient reason keeps the block
// for the next cycle; a map the contract refused on chain does not hold it.
func TestHandleMap_TransientFailureKeepsTheBlock(t *testing.T) {
	bot, gql, caller, state, addr, _ := newTestBotWithMocks()
	depositAddr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[depositAddr] = "deposit_to=hive:testuser"
	blockBytes := buildTestBlock(t, depositAddr, &chaincfg.TestNet4Params)
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))
	gql.lastHeight = "200"

	// The exact transient failure seen on testnet: the RC pre-flight refused.
	caller.err = errors.New("insufficient RC for map: 8333 available, 10000 needed for this op alone")
	assert.False(t, bot.HandleMap(blockBytes, 100))
	h, _ := state.GetBlockHeight(context.Background())
	assert.Equal(t, uint64(100), h, "the deposit's block must be retried, not skipped")

	// Credits restored: the same block is mapped and the cursor advances.
	caller.err = nil
	gql.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}
	assert.True(t, bot.HandleMap(blockBytes, 100))
	h, _ = state.GetBlockHeight(context.Background())
	assert.Equal(t, uint64(101), h)
}

func TestHandleMap_OnChainRefusalDoesNotHoldTheBlock(t *testing.T) {
	bot, gql, _, state, addr, _ := newTestBotWithMocks()
	failed := newMockFailedTxStore()
	bot.FailedTxDB = failed
	depositAddr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[depositAddr] = "deposit_to=hive:testuser"
	blockBytes := buildTestBlock(t, depositAddr, &chaincfg.TestNet4Params)
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))
	gql.lastHeight = "200"
	gql.txStatuses = map[string]string{"mock-tx-id": "FAILED"}

	assert.True(t, bot.HandleMap(blockBytes, 100))
	h, _ := state.GetBlockHeight(context.Background())
	assert.Equal(t, uint64(101), h)
	all, _ := failed.GetAll(context.Background())
	assert.Len(t, all, 1, "the refused map is kept for /retry")
}

// The node refuses any L2 tx above MAX_TX_SIZE, and a map carries the whole
// deposit tx: a deposit inside a large Bitcoin tx can never be mapped. That refusal
// must not hold the block (every later deposit would stall behind it); it is
// recorded for the operator and the cursor moves on.
func TestHandleMap_TooLargeForThePoolDoesNotHoldTheBlock(t *testing.T) {
	bot, gql, caller, state, addr, _ := newTestBotWithMocks()
	failed := newMockFailedTxStore()
	bot.FailedTxDB = failed
	depositAddr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[depositAddr] = "deposit_to=hive:testuser"
	blockBytes := buildTestBlock(t, depositAddr, &chaincfg.TestNet4Params)
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))
	gql.lastHeight = "200"
	caller.err = errors.New("submit transaction: transaction size too big 21890 > 16384")

	assert.True(t, bot.HandleMap(blockBytes, 100))
	h, _ := state.GetBlockHeight(context.Background())
	assert.Equal(t, uint64(101), h, "an oversized map must not hold the block")
	all, _ := failed.GetAll(context.Background())
	require.Len(t, all, 1, "the uncreditable deposit is recorded for the operator")
	assert.True(t, strings.HasPrefix(all[0].TxId, "map-too-large-"), all[0].TxId)
	assert.Equal(t, "map", all[0].Action)
}
