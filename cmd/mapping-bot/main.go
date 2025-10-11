package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mapper"
	"vsc-node/cmd/mapping-bot/mempool"

	"github.com/hasura/go-graphql-client"
	flatfs "github.com/ipfs/go-ds-flatfs"
)

const (
	graphQLUrl = "https://api.vsc.eco/api/v1/graphql"
	httpPort   = 8000
)

func newDataStore(path string) (*flatfs.Datastore, error) {
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, err
	}

	// uses default sharding
	fs, err := flatfs.CreateOrOpen(path, flatfs.NextToLast(2), false)
	if err != nil {
		return nil, err
	}

	return fs, nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := database.New("./wallet_address")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create datastore: %s\n", err.Error())
		os.Exit(1)
	}

	go mapBotHttpServer(ctx, db, httpPort)

	datastore, err := newDataStore("./map-bot-data")
	if err != nil {
		log.Fatalln(err.Error())
	}

	bot, err := mapper.NewMapperState(datastore)
	if err != nil {
		log.Fatalln(err.Error())
	}
	mempoolClient := mempool.NewMempoolClient()
	graphQlClient := graphql.NewClient(graphQLUrl, nil)
	for {
		observedTxs, txSpends, err := mapper.FetchContractData(graphQlClient)
		if err != nil {
			fmt.Println(err.Error())
			time.Sleep(time.Minute)
		} else {
			bot.Mutex.Lock()
			bot.ObservedTxs = observedTxs
			bot.Mutex.Unlock()
			go bot.HandleUnmap(mempoolClient, txSpends)
		}

		blockHeight := bot.LastBlockHeight + 1

		hash, status, err := mempoolClient.GetBlockHashAtHeight(blockHeight)
		if status == http.StatusNotFound {
			fmt.Println("No new block.")
			time.Sleep(time.Minute)
			continue
		} else if err != nil {
			fmt.Println(err.Error())
			time.Sleep(time.Minute)
			continue
		}
		blockBytes, err := mempoolClient.GetRawBlock(hash)
		if err != nil {
			fmt.Println(err.Error())
			time.Sleep(time.Minute)
			continue
		}

		go bot.HandleMap(blockBytes, blockHeight)
		time.Sleep(time.Minute)
	}

}
