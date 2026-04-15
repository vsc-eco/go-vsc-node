package mapper

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"
	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/hasura/go-graphql-client"
)

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
	chainCfg := chain.NewBTCTestnet4(http.DefaultClient)
	return &Bot{
		Db:          db,
		gqlURLs:     []string{defaultGraphQLUrl},
		gqlClients:  []*graphql.Client{graphql.NewClient(defaultGraphQLUrl, http.DefaultClient)},
		Chain:       chainCfg,
		ChainParams: chainCfg.ChainParams,
		BotConfig:   cfg,
		L:           slog.Default(),
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
