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
)

const httpPort = 8000

func main() {
	db, err := database.New(context.Background(), "mongodb://localhost:27017", "mappingbot")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create datastore: %s\n", err.Error())
		os.Exit(1)
	}
	defer db.Close(context.Background())
	lastClear := time.Now()

	// remove for prod
	err = db.Addresses.Insert(
		context.TODO(),
		"tb1qeej59j0rjgkdh9kae4hjpljevzr3tjlndn33aywncs5uhf8swe2s67hvfy",
		"deposit_to=hive:milo-hpr",
	)
	if err != nil {
		if err != database.ErrAddrExists {
			fmt.Fprintf(os.Stderr, "failed to add default address\n")
			os.Exit(1)
		}
	}
	err = db.State.SetBlockHeight(context.TODO(), 4806875)
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
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		// clear address db if it's been 24 hours
		if time.Since(lastClear).Hours() > 24 {
			_, err := db.Addresses.DeleteOlderThan(ctx, 24*30*time.Hour)
			if err != nil {
				// don't need to break/continue since it's not a critical error
				fmt.Fprintf(os.Stderr, "error deleting expired addresses: %s\n", err.Error())
			}
		}

		if err != nil {
			fmt.Fprintf(os.Stderr, "error fetching tx spends: %s\n", err.Error())
			cancel()
			time.Sleep(time.Minute)
			continue
		} else {
			go bot.HandleUnmap(mempoolClient)
		}

		blockHeight, err := bot.Db.State.GetBlockHeight(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error fetching block height from db: %s", err.Error())
			continue
		}

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

		// // TODO: remove for prod
		// time.Sleep(3 * time.Second)
		// return
		cancel()
		time.Sleep(time.Minute)
	}
}
