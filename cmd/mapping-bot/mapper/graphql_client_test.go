package mapper

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mempool"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/hasura/go-graphql-client"
)

// noopTestMempoolClient satisfies MempoolClientIface without hitting any network.
type noopTestMempoolClient struct{}

func (n *noopTestMempoolClient) PostTx(_ string) error                             { return nil }
func (n *noopTestMempoolClient) GetAddressTxs(_ string) ([]mempool.Transaction, error) {
	return nil, nil
}
func (n *noopTestMempoolClient) GetRawBlock(_ string) ([]byte, error)               { return nil, nil }
func (n *noopTestMempoolClient) GetBlockHashAtHeight(_ uint64) (string, int, error) { return "", 0, nil }
func (n *noopTestMempoolClient) GetTipHeight() (uint64, error)                      { return 0, nil }

// newIntegrationBot creates a Bot pointing at the real VSC API.
func newIntegrationBot(t *testing.T) *Bot {
	t.Helper()
	db, err := database.New(context.Background(), "mongodb://localhost:27017", "mappingbottest_gql")
	if err != nil {
		t.Skipf("MongoDB not available: %s", err)
	}
	t.Cleanup(func() {
		db.DropDatabase(context.Background())
		db.Close(context.Background())
	})
	cfg := NewMappingBotConfig()
	return &Bot{
		Db:            db,
		GqlClient:     graphql.NewClient(defaultGraphQLUrl, http.DefaultClient),
		ChainParams:   &chaincfg.TestNet4Params,
		BotConfig:     cfg,
		L:             slog.Default(),
		MempoolClient: &noopTestMempoolClient{},
	}
}

func TestSignatures_Integration(t *testing.T) {
	t.Skip("integration test: requires live VSC API")
	bot := newIntegrationBot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := bot.FetchSignatures(ctx, []string{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(result)
}

func TestTxSpends_Integration(t *testing.T) {
	t.Skip("integration test: requires live VSC API")
	bot := newIntegrationBot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := bot.FetchTxSpends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("result", result)
}

func TestLastHeight_Integration(t *testing.T) {
	t.Skip("integration test: requires live VSC API")
	bot := newIntegrationBot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := bot.FetchLastHeight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("result", result)
}
