package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mapper"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db"
	"vsc-node/modules/hive/streamer"
)

const httpPort = 8000

func main() {
	args, err := parseArgs()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	if args.debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}
	slog.Debug("vsc network", "network", args.network)

	sysConfig := systemconfig.FromNetwork(args.network)

	mappingBotConfig := mapper.NewMappingBotConfig(args.dataDir)
	identityConfig := common.NewIdentityConfig(args.dataDir)
	hiveConfig := streamer.NewHiveConfig(args.dataDir)
	dbConfig := db.NewDbConfig(args.dataDir)

	configs := aggregate.New([]aggregate.Plugin{
		mappingBotConfig,
		identityConfig,
		hiveConfig,
		dbConfig,
	})

	if err := configs.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init configs: %s\n", err.Error())
		os.Exit(1)
	}

	if dbConfig.GetDbName() == db.DefaultDbName {
		dbConfig.SetDbName("btc-mapping-bot")
	}

	if args.isInit {
		fmt.Printf("config initialized at %s\n", args.dataDir)
		return
	}

	contractId := mappingBotConfig.Get().ContractId
	if contractId == "" || contractId == "ADD_BTC_MAPPING_CONTRACT_ID" {
		fmt.Fprintf(os.Stderr, "ContractId must be set in %s\n", args.dataDir)
		os.Exit(1)
	}

	fmt.Printf("contractId set to: %s\n", contractId)

	db, err := database.New(context.Background(), dbConfig.Get().DbURI, dbConfig.GetDbName())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create datastore: %s\n", err.Error())
		os.Exit(1)
	}
	defer db.Close(context.Background())
	lastClear := time.Now()

	bot, err := mapper.NewBot(db, args.btcNetwork, mappingBotConfig, identityConfig, hiveConfig, sysConfig)
	if err != nil {
		log.Fatalln(err.Error())
	}

	httpCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mapBotHttpServer(httpCtx, db.Addresses, httpPort, bot)

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
			time.Sleep(time.Minute)
			cancel()
			continue
		} else {
			go bot.HandleUnmap()
		}

		blockHeight, err := bot.Db.State.GetBlockHeight(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error fetching block height from db: %s", err.Error())
			cancel()
			continue
		}

		if blockHeight == 0 {
			startHeight, err := bot.MempoolClient.GetTipHeight()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error fetching tip height from mempool: %s\n", err.Error())
				time.Sleep(time.Minute)
				cancel()
				continue
			}
			fmt.Printf("no stored block height, starting from tip: %d\n", startHeight)

			if err := bot.Db.State.SetBlockHeight(ctx, startHeight); err != nil {
				fmt.Fprintf(os.Stderr, "error seeding block height: %s\n", err.Error())
				time.Sleep(time.Minute)
				cancel()
				continue
			}
			blockHeight = startHeight
		}

		hash, status, err := bot.MempoolClient.GetBlockHashAtHeight(blockHeight)
		if status == http.StatusNotFound {
			fmt.Println("No new block.")
			time.Sleep(time.Minute)
			cancel()
			continue
		} else if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			// return
			time.Sleep(time.Minute)
			cancel()
			continue
		}
		blockBytes, err := bot.MempoolClient.GetRawBlock(hash)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			// return
			time.Sleep(time.Minute)
			cancel()
			continue
		}

		go bot.HandleMap(blockBytes, blockHeight)

		// // TODO: remove for prod
		// time.Sleep(3 * time.Second)
		// return
		cancel()
		if args.btcNetwork == "mainnet" {
			time.Sleep(time.Minute)
		} else {
			time.Sleep(10 * time.Second)
		}
	}
}
