package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mapper"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db"
	"vsc-node/modules/hive/streamer"
)

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

	// DB name set below after chain resolution

	if args.isInit {
		fmt.Printf("config initialized at %s\n", args.dataDir)
		return
	}

	// CLI port override
	if args.port > 0 {
		mappingBotConfig.SetHttpPort(uint16(args.port))
	}

	contractId := mappingBotConfig.ContractId()
	if contractId == "" || contractId == "ADD_MAPPING_CONTRACT_ID" {
		fmt.Fprintf(os.Stderr, "ContractId must be set in %s\n", args.dataDir)
		os.Exit(1)
	}

	// Resolve chain configuration from CLI flags
	chainCfg, err := chain.Resolve(args.chainName, args.chainNetwork, http.DefaultClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unsupported chain: %s\n", err.Error())
		os.Exit(1)
	}

	// Use chain-specific default DB name if not overridden
	if dbConfig.GetDbName() == db.DefaultDbName || dbConfig.GetDbName() == "btc-mapping-bot" {
		dbConfig.SetDbName(chainCfg.DefaultDbName)
	}

	slog.Debug(
		"params",
		"vsc network", sysConfig.NetId(),
		"hive chain id", sysConfig.HiveChainId(),
		"chain", chainCfg.Name,
		"chain network", args.chainNetwork,
		"contract id", mappingBotConfig.ContractId(),
		"connected graphql", mappingBotConfig.DefaultValue().ConnectedGraphQLAddr,
	)

	db, err := database.New(context.Background(), dbConfig.Get().DbURI, dbConfig.GetDbName())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create datastore: %s\n", err.Error())
		os.Exit(1)
	}
	defer db.Close(context.Background())
	lastClear := time.Now()

	bot, err := mapper.NewBot(db, chainCfg, mappingBotConfig, identityConfig, hiveConfig, sysConfig)
	if err != nil {
		slog.Error("could not initialize new bot", "err", err.Error())
		os.Exit(1)
	}

	httpCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mapBotHttpServer(httpCtx, db.Addresses, bot)

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		// Periodic cleanup (every 24h)
		if time.Since(lastClear).Hours() > 24 {
			_, err := db.Addresses.DeleteOlderThan(ctx, 24*30*time.Hour)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error deleting expired addresses: %s\n", err.Error())
			}
			// Clean up txs stuck in "sent" state for > 7 days (likely failed broadcasts)
			_, err = db.State.DeleteOldSentTransactions(ctx, 7*24*time.Hour)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error deleting old sent transactions: %s\n", err.Error())
			}
			lastClear = time.Now()
		}

		// Run sequentially to avoid races on shared DB state.
		// HandleUnmap processes pending txs; HandleConfirmations promotes sent → confirmed.
		bot.HandleUnmap()
		bot.HandleConfirmations()

		blockHeight, err := bot.Db.State.GetBlockHeight(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error fetching block height from db: %s", err.Error())
			cancel()
			continue
		}

		if blockHeight == 0 {
			startHeight, err := bot.Chain.Client.GetTipHeight()
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

		hash, status, err := bot.Chain.Client.GetBlockHashAtHeight(blockHeight)
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
		blockBytes, err := bot.Chain.Client.GetRawBlock(hash)
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
		// Sleep based on chain's expected block interval
		time.Sleep(chainCfg.BlockInterval)
	}
}
