package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mapper"
	"vsc-node/cmd/mapping-bot/mempool"

	"github.com/btcsuite/btcd/chaincfg"
)

const httpPort = 8000

func parseNetwork(name string) (*chaincfg.Params, string, error) {
	switch name {
	case "mainnet":
		return &chaincfg.MainNetParams, mempool.MempoolMainnetAPIBase, nil
	case "testnet4":
		return &chaincfg.TestNet4Params, mempool.MempoolTestnet4APIBase, nil
	case "testnet3":
		return &chaincfg.TestNet3Params, mempool.MempoolTestnet3APIBase, nil
	case "regnet":
		return &chaincfg.RegressionNetParams, mempool.MempoolMainnetAPIBase, nil
	default:
		return nil, "", fmt.Errorf("unknown network %q: must be mainnet, testnet4, testnet3, or regnet", name)
	}
}

func main() {
	a, err := parseArgs()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	chainParams, mempoolBase, err := parseNetwork(a.network)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	network = chainParams

	cfg := newMappingBotConfig(a.dataDir)
	if err := cfg.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %s\n", err.Error())
		os.Exit(1)
	}

	if a.isInit {
		fmt.Printf("config initialized at %s\n", cfg.FilePath())
		return
	}

	contractinterface.ContractId = cfg.GetContractId()
	if contractinterface.ContractId == "" || contractinterface.ContractId == "ADD_BTC_MAPPING_CONTRACT_ID" {
		fmt.Fprintf(os.Stderr, "ContractId must be set in %s\n", cfg.FilePath())
		os.Exit(1)
	}

	mongoURL := os.Getenv("MONGO_URL")
	if mongoURL == "" {
		mongoURL = "mongodb://localhost:27017"
	}
	db, err := database.New(context.Background(), mongoURL, "mappingbot")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create datastore: %s\n", err.Error())
		os.Exit(1)
	}
	defer db.Close(context.Background())
	lastClear := time.Now()

	bot, err := mapper.NewMapperState(db, chainParams, mempoolBase)
	if err != nil {
		log.Fatalln(err.Error())
	}

	httpCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mapBotHttpServer(httpCtx, db.Addresses, httpPort, bot)

	mempoolClient := mempool.NewMempoolClient(http.DefaultClient, mempoolBase)
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
			go bot.HandleUnmap(mempoolClient)
		}

		blockHeight, err := bot.Db.State.GetBlockHeight(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error fetching block height from db: %s", err.Error())
			cancel()
			continue
		}

		if blockHeight == 0 {
			// prefer the contract's last processed height so we don't re-scan blocks
			// the contract has already seen; fall back to the mempool tip
			var startHeight uint32
			contractHeightStr, err := mapper.FetchLastHeight(ctx, bot.GqlClient)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error fetching last height from contract: %s\n", err.Error())
			} else if contractHeightStr != "" {
				h, err := strconv.ParseUint(contractHeightStr, 10, 32)
				if err != nil {
					fmt.Fprintf(os.Stderr, "contract last height is not a valid integer %q: %s\n", contractHeightStr, err.Error())
				} else {
					startHeight = uint32(h)
					fmt.Printf("no stored block height, resuming from contract height: %d\n", startHeight)
				}
			}
			if startHeight == 0 {
				startHeight, err = mempoolClient.GetTipHeight()
				if err != nil {
					fmt.Fprintf(os.Stderr, "error fetching tip height from mempool: %s\n", err.Error())
					time.Sleep(time.Minute)
					cancel()
					continue
				}
				fmt.Printf("no stored block height, starting from tip: %d\n", startHeight)
			}
			if err := bot.Db.State.SetBlockHeight(ctx, startHeight); err != nil {
				fmt.Fprintf(os.Stderr, "error seeding block height: %s\n", err.Error())
				time.Sleep(time.Minute)
				cancel()
				continue
			}
			blockHeight = startHeight
		}

		hash, status, err := mempoolClient.GetBlockHashAtHeight(blockHeight)
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
		blockBytes, err := mempoolClient.GetRawBlock(hash)
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
		time.Sleep(time.Minute)
	}
}
