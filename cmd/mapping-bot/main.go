package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
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
		slog.Error("failed to parse args", "err", err)
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
		slog.Error("failed to init configs", "err", err)
		os.Exit(1)
	}

	// DB name set below after chain resolution

	if args.isInit {
		slog.Info("config initialized", "path", args.dataDir)
		return
	}

	// CLI port override
	if args.port > 0 {
		mappingBotConfig.SetHttpPort(uint16(args.port))
	}

	contractId := mappingBotConfig.ContractId()
	if contractId == "" || contractId == "ADD_MAPPING_CONTRACT_ID" {
		slog.Error("ContractId must be set", "path", args.dataDir)
		os.Exit(1)
	}

	// Resolve chain configuration from CLI flags
	chainCfg, err := chain.Resolve(args.chainName, args.chainNetwork, http.DefaultClient)
	if err != nil {
		slog.Error("unsupported chain", "err", err)
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
		slog.Error("failed to create datastore", "err", err)
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

	var wg sync.WaitGroup
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		// Periodic cleanup (every 24h)
		if time.Since(lastClear).Hours() > 24 {
			_, err := db.Addresses.DeleteOlderThan(ctx, 24*30*time.Hour)
			if err != nil {
				bot.L.Error("error deleting expired addresses", "err", err)
			}
			// Clean up txs stuck in "sent" state for > 7 days (likely failed broadcasts)
			_, err = db.State.DeleteOldSentTransactions(ctx, 7*24*time.Hour)
			if err != nil {
				bot.L.Error("error deleting old sent transactions", "err", err)
			}
			lastClear = time.Now()
		}

		blockHeight, err := bot.Db.State.GetBlockHeight(ctx)
		if err != nil {
			bot.L.Error("error fetching block height from db", "err", err)
			cancel()
			continue
		}

		if blockHeight == 0 {
			startHeight, err := bot.Chain.Client.GetTipHeight()
			if err != nil {
				bot.L.Error("error fetching tip height", "err", err)
				time.Sleep(chainCfg.SleepInterval)
				cancel()
				continue
			}
			bot.L.Info("no stored block height, starting from tip", "height", startHeight)

			if err := bot.Db.State.SetBlockHeight(ctx, startHeight); err != nil {
				bot.L.Error("error seeding block height", "err", err)
				time.Sleep(chainCfg.SleepInterval)
				cancel()
				continue
			}
			blockHeight = startHeight
		}

		hash, status, err := bot.Chain.Client.GetBlockHashAtHeight(blockHeight)
		if status == http.StatusNotFound {
			bot.L.Info("no new block")
			// At head — still run unmap/confirmations, then sleep before checking again
			bot.HandleUnmap()
			bot.HandleConfirmations()
			time.Sleep(chainCfg.SleepInterval)
			cancel()
			continue
		} else if err != nil {
			bot.L.Error("error fetching block hash", "height", blockHeight, "err", err)
			time.Sleep(chainCfg.SleepInterval)
			cancel()
			continue
		}
		blockBytes, err := bot.Chain.Client.GetRawBlock(hash)
		if err != nil {
			bot.L.Error("error fetching raw block", "hash", hash, "err", err)
			time.Sleep(chainCfg.SleepInterval)
			cancel()
			continue
		}

		// Run map and unmap concurrently, but wait for both to finish before
		// advancing to the next block.
		var mapped bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			mapped = bot.HandleMap(blockBytes, blockHeight)
		}()
		go func() {
			defer wg.Done()
			bot.HandleUnmap()
			bot.HandleConfirmations()
		}()
		wg.Wait()

		cancel()
		// If the block wasn't processed (e.g., not yet in the contract), sleep before retrying.
		if !mapped {
			time.Sleep(chainCfg.SleepInterval)
		}
	}
}
