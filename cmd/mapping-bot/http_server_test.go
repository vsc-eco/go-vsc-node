package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"vsc-node/cmd/mapping-bot/chain"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mapper"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/hasura/go-graphql-client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testBotConfig is a minimal BotConfiger implementation for tests.
type testBotConfig struct {
	primaryKey string
	backupKey  string
}

func (c *testBotConfig) ContractId() string { return "test-contract" }
func (c *testBotConfig) PrimaryKey() string { return c.primaryKey }
func (c *testBotConfig) BackupKey() string  { return c.backupKey }
func (c *testBotConfig) HttpPort() uint16   { return 8000 }
func (c *testBotConfig) SignApiKey() string { return "test-api-key" }
func (c *testBotConfig) FilePath() string   { return "test-config.json" }
func (c *testBotConfig) RcLimit() uint      { return 10000 }

// noopChainClient prevents any real blockchain calls from being made.
type noopChainClient struct {
	posted []string
}

func (n *noopChainClient) PostTx(rawTx string) error {
	n.posted = append(n.posted, rawTx)
	return nil
}
func (n *noopChainClient) GetAddressTxs(_ string) ([]chain.TxHistoryEntry, error) {
	return nil, nil
}
func (n *noopChainClient) GetRawBlock(_ string) ([]byte, error)               { return nil, nil }
func (n *noopChainClient) GetBlockHashAtHeight(_ uint64) (string, int, error) { return "", 0, nil }
func (n *noopChainClient) GetTipHeight() (uint64, error)                      { return 0, nil }
func (n *noopChainClient) GetTxStatus(_ string) (bool, error)                 { return false, nil }
func (n *noopChainClient) GetTxDetails(string) (chain.TxConfirmationDetails, error) {
	return chain.TxConfirmationDetails{}, nil
}

// Compressed secp256k1 public keys used only for tests (generator point G and 2G).
const (
	testPrimaryKeyHex = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	testBackupKeyHex  = "02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
)

// mockGQLServer returns an httptest.Server that responds to FetchPublicKeys queries
// with the test public keys, and returns empty state for everything else.
func mockGQLServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"getStateByKeys":{%q:%q,%q:%q}}}`,
			"pubkey", testPrimaryKeyHex,
			"backupkey", testBackupKeyHex,
		)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestBot(t *testing.T, db *database.Database) *mapper.Bot {
	t.Helper()
	gqlSrv := mockGQLServer(t)
	testChain := &chain.ChainConfig{
		Name:        "btc",
		AssetSymbol: "BTC",
		Client:      &noopChainClient{},
		AddressGen:  &chain.BTCAddressGenerator{Params: &chaincfg.TestNet4Params, BackupCSVBlocks: 2},
		ChainParams: &chaincfg.TestNet4Params,
	}
	return &mapper.Bot{
		Db:          db,
		GqlClient:   graphql.NewClient(gqlSrv.URL, gqlSrv.Client()),
		Chain:       testChain,
		ChainParams: testChain.ChainParams,
		BotConfig: &testBotConfig{
			primaryKey: testPrimaryKeyHex,
			backupKey:  testBackupKeyHex,
		},
		L: slog.Default(),
	}
}

func setupTestDatabase(t *testing.T) *database.Database {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(ctx, "mongodb://localhost:27017", "mappingbottest_http")
	if err != nil {
		t.Skipf("MongoDB not available: %s", err)
	}
	t.Cleanup(func() {
		db.DropDatabase(context.Background())
		db.Close(context.Background())
	})
	return db
}

func TestRequestHandler(t *testing.T) {
	db := setupTestDatabase(t)
	bot := newTestBot(t, db)

	instruction := "deposit_to=hive:sudo-sandwich"

	primaryKey, err := hex.DecodeString(testPrimaryKeyHex)
	require.NoError(t, err)
	backupKey, err := hex.DecodeString(testBackupKeyHex)
	require.NoError(t, err)

	tag := sha256.Sum256([]byte(instruction))
	expectedAddr, _, err := createP2WSHAddressWithBackup(primaryKey, backupKey, tag[:], &chaincfg.TestNet4Params)
	require.NoError(t, err)

	body := requestBody{Instruction: instruction}

	t.Run("201 created", func(t *testing.T) {
		req := jsonRequest(t, http.MethodPost, "/", body)
		w := httptest.NewRecorder()
		requestHandler(t.Context(), bot).ServeHTTP(w, req)
		assert.Equal(t, http.StatusCreated, w.Code)

		stored, err := db.Addresses.GetInstruction(t.Context(), expectedAddr)
		require.NoError(t, err)
		assert.Equal(t, instruction, stored)
	})

	t.Run("409 conflict on duplicate", func(t *testing.T) {
		req := jsonRequest(t, http.MethodPost, "/", body)
		w := httptest.NewRecorder()
		requestHandler(t.Context(), bot).ServeHTTP(w, req)
		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("400 bad request missing instruction", func(t *testing.T) {
		req := jsonRequest(t, http.MethodPost, "/", map[string]string{})
		w := httptest.NewRecorder()
		requestHandler(t.Context(), bot).ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("405 method not allowed", func(t *testing.T) {
		req := jsonRequest(t, http.MethodGet, "/", body)
		w := httptest.NewRecorder()
		requestHandler(t.Context(), bot).ServeHTTP(w, req)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})
}

func TestSignHandler(t *testing.T) {
	db := setupTestDatabase(t)
	bot := newTestBot(t, db)

	sigHash := make([]byte, 32)
	sigHash[0] = 0xAB
	fakeSigHex := hex.EncodeToString(make([]byte, 64))

	require.NoError(t, db.State.AddPendingTransaction(
		t.Context(),
		"txSignTest",
		[]byte{0x01, 0x02},
		[]contractinterface.UnsignedSigHash{
			{Index: 0, SigHash: sigHash, WitnessScript: []byte{0xDE, 0xAD}},
		},
	))

	t.Run("200 signature applied", func(t *testing.T) {
		body := map[string]any{
			"tx_id": "txSignTest",
			"signatures": []map[string]any{
				{"index": 0, "signature": fakeSigHex},
			},
		}
		req := jsonRequest(t, http.MethodPost, "/sign", body)
		w := httptest.NewRecorder()
		signHandler(t.Context(), bot).ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("404 tx not found", func(t *testing.T) {
		body := map[string]any{
			"tx_id": "doesNotExist",
			"signatures": []map[string]any{
				{"index": 0, "signature": fakeSigHex},
			},
		}
		req := jsonRequest(t, http.MethodPost, "/sign", body)
		w := httptest.NewRecorder()
		signHandler(t.Context(), bot).ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("400 index out of range", func(t *testing.T) {
		require.NoError(t, db.State.AddPendingTransaction(
			t.Context(),
			"txOOR",
			[]byte{0x01},
			[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: make([]byte, 32), WitnessScript: []byte{0x01}}},
		))
		body := map[string]any{
			"tx_id": "txOOR",
			"signatures": []map[string]any{
				{"index": 99, "signature": fakeSigHex},
			},
		}
		req := jsonRequest(t, http.MethodPost, "/sign", body)
		w := httptest.NewRecorder()
		signHandler(t.Context(), bot).ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("400 invalid hex signature", func(t *testing.T) {
		require.NoError(t, db.State.AddPendingTransaction(
			t.Context(),
			"txBadHex",
			[]byte{0x01},
			[]contractinterface.UnsignedSigHash{{Index: 0, SigHash: make([]byte, 32), WitnessScript: []byte{0x01}}},
		))
		body := map[string]any{
			"tx_id": "txBadHex",
			"signatures": []map[string]any{
				{"index": 0, "signature": "not-hex!"},
			},
		}
		req := jsonRequest(t, http.MethodPost, "/sign", body)
		w := httptest.NewRecorder()
		signHandler(t.Context(), bot).ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("400 missing required fields", func(t *testing.T) {
		req := jsonRequest(t, http.MethodPost, "/sign", map[string]any{})
		w := httptest.NewRecorder()
		signHandler(t.Context(), bot).ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestHealthHandler(t *testing.T) {
	db := setupTestDatabase(t)
	bot := newTestBot(t, db)

	req, err := http.NewRequest(http.MethodGet, "/health", nil)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	healthHandler(bot).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp healthResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "starting", resp.Status)
}

// --- helpers ---

func jsonRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	buf := &bytes.Buffer{}
	require.NoError(t, json.NewEncoder(buf).Encode(body))
	req, err := http.NewRequest(method, path, buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return req
}
