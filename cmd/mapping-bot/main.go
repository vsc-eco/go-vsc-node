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

	addressDb, err := database.New("./wallet-address-datastore")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create datastore: %s\n", err.Error())
		os.Exit(1)
	}

	go mapBotHttpServer(ctx, addressDb, httpPort)

	generalDb, err := newDataStore("./map-bot-datastore")
	if err != nil {
		log.Fatalln(err.Error())
	}

	bot, err := mapper.NewMapperState(generalDb)
	if err != nil {
		log.Fatalln(err.Error())
	}
	mempoolClient := mempool.NewMempoolClient()
	graphQlClient := graphql.NewClient(graphQLUrl, nil)
	for {
		observedTxs, txSpends, err := mapper.FetchContractData(graphQlClient)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return
		} else {
			bot.Mutex.Lock()
			bot.ObservedTxs = observedTxs
			bot.Mutex.Unlock()
			go bot.HandleUnmap(mempoolClient, graphQlClient, txSpends)
		}

		blockHeight := bot.LastBlockHeight + 1

		hash, status, err := mempoolClient.GetBlockHashAtHeight(blockHeight)
		if status == http.StatusNotFound {
			fmt.Println("No new block.")
			time.Sleep(time.Minute)
			continue
		} else if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			// return
			time.Sleep(time.Minute)
			continue
		}
		blockBytes, err := mempoolClient.GetRawBlock(hash)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			// return
			time.Sleep(time.Minute)
			continue
		}

		go bot.HandleMap(blockBytes, blockHeight, addressDb)
		// TODO: remove for prod
		// time.Sleep(3 * time.Second)
		// return
		time.Sleep(time.Minute)
	}
}
