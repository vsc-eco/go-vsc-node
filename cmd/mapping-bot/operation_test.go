package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"os"
	"testing"
	"time"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mapper"
	"vsc-node/cmd/mapping-bot/mempool"

	"github.com/hasura/go-graphql-client"
)

func TestUnmap(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	db, err := database.New(context.Background(), "mongodb://localhost:27017", "mappingbottest")
	if err != nil {
		t.Fatalf("failed to create datastore: %s\n", err.Error())
	}
	defer db.DropDatabase(context.Background())

	// SET THIS TO LAST BLOCK HEIGHT FOR TEST
	err = db.State.SetBlockHeight(context.TODO(), 4806875)
	if err != nil {
		slog.Error("failed to add default block height\n")
		os.Exit(1)
	}

	bot, err := mapper.NewMapperState(db)
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	loggingClient := &http.Client{
		Transport: &loggingTransport{http.DefaultTransport},
	}
	bot.GqlClient = graphql.NewClient("https://api.vsc.eco/api/v1/graphql", loggingClient)

	mempoolClient := mempool.NewMempoolClient(loggingClient)

	if err != nil {
		t.Fatalf("error fetching tx spends: %s\n", err.Error())
	} else {
		bot.HandleUnmap(mempoolClient)
	}
}

func TestMap(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	db, err := database.New(context.Background(), "mongodb://localhost:27017", "mappingbottest")
	if err != nil {
		t.Fatalf("failed to create datastore: %s\n", err.Error())
	}
	defer db.DropDatabase(context.Background())

	// remove for prod
	err = db.Addresses.Insert(
		context.TODO(),
		"tb1q9gxwgzzxs7d597nh8843tndtwl9qrdup02tc0xcltrlt2tjyg7xqhat2zx",
		"deposit_to=hive:milo-hpr",
	)
	if err != nil {
		if err != database.ErrAddrExists {
			fmt.Fprintf(os.Stderr, "failed to add default address\n")
			os.Exit(1)
		}
	}
	err = db.State.SetBlockHeight(context.TODO(), 114810)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to add default block height\n")
		os.Exit(1)
	}

	bot, err := mapper.NewMapperState(db)
	if err != nil {
		log.Fatalln(err.Error())
	}

	httpCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mapBotHttpServer(httpCtx, db.Addresses, httpPort, bot)

	mempoolClient := mempool.NewMempoolClient(http.DefaultClient)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	blockHeight, err := bot.Db.State.GetBlockHeight(ctx)
	if err != nil {
		t.Fatalf("error fetching block height from db: %s", err.Error())
	}

	hash, status, err := mempoolClient.GetBlockHashAtHeight(blockHeight)
	if status == http.StatusNotFound {
		t.Fatalf("No new block.")
	} else if err != nil {
		t.Fatal(err.Error())
	}
	blockBytes, err := mempoolClient.GetRawBlock(hash)
	if err != nil {
		t.Fatal(err.Error())
	}

	bot.HandleMap(blockBytes, blockHeight)
}

// Logging transport
type loggingTransport struct {
	transport http.RoundTripper
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqDump, _ := httputil.DumpRequestOut(req, true)
	fmt.Printf("Request:\n%s\n\n", reqDump)

	resp, err := t.transport.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	respDump, _ := httputil.DumpResponse(resp, true)
	fmt.Printf("Response:\n%s\n\n", respDump)

	return resp, err
}
