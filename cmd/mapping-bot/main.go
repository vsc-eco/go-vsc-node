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

	flatfs "github.com/ipfs/go-ds-flatfs"
)

const httpPort = 8000

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
	addressDb, err := database.New(context.Background(), "mongodb://localhost:27017", "mappingbot", "address_mappings")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create datastore: %s\n", err.Error())
		os.Exit(1)
	}
	defer addressDb.Close(context.Background())
	lastClear := time.Now()

	// remove for prod
	// err = addressDb.InsertAddressMap(
	// 	context.TODO(),
	// 	"tb1qvzwxaadfvqrc4n50yam2clw3hdvj2s6028vfmf0t3725yj0q0ftsq589fm",
	// 	"deposit_to=hive:milo-hpr",
	// )
	//

	if err != nil {
		if err != database.ErrAddrExists {
			fmt.Fprintf(os.Stderr, "failed to add default address\n")
			os.Exit(1)
		}
	}

	generalDb, err := newDataStore("./map-bot-datastore")
	if err != nil {
		log.Fatalln(err.Error())
	}

	bot, err := mapper.NewMapperState(generalDb, addressDb)
	if err != nil {
		log.Fatalln(err.Error())
	}

	httpCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mapBotHttpServer(httpCtx, addressDb, httpPort, bot)

	mempoolClient := mempool.NewMempoolClient()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		// clear address db if it's been 24 hours
		if time.Since(lastClear).Hours() > 24 {
			_, err := addressDb.DeleteOlderThan(ctx, 24*30*time.Hour)
			if err != nil {
				// don't need to break/continue since it's not a critical error
				fmt.Fprintf(os.Stderr, "error deleting expired addresses: %s\n", err.Error())
			}
		}

		txSpends, err := mapper.FetchTxSpends(ctx, bot.GqlClient)
		// cancel context before handling error since it has to be cleaned up either way
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error fetching tx spends: %s\n", err.Error())
			time.Sleep(time.Minute)
			continue
		} else {
			go bot.HandleUnmap(mempoolClient, txSpends)
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

		go bot.HandleMap(blockBytes, blockHeight)

		cancel()
		// // TODO: remove for prod
		// time.Sleep(3 * time.Second)
		// return
		time.Sleep(time.Minute)
	}
}
