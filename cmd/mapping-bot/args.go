package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	isInit        bool
	debug         bool
	network       string
	chainName     string
	chainNetwork  string
	dataDir       string
	port          int
	sysconfigPath string
	chainAPI      string
}

func parseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("VSC Mapping Bot - Monitors a blockchain and submits mapping transactions.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	isInit := flag.Bool("init", false, "Initialize the bot (create config file and exit)")
	debug := flag.Bool("debug", false, "Enable debug logging")
	network := flag.String("network", "mainnet", "VSC network: mainnet, testnet, or devnet")
	chainName := flag.String("chain", "btc", "Chain to monitor: btc, ltc, dash, doge, bch")
	chainNetwork := flag.String("chain-network", "mainnet", "Chain network: mainnet, testnet, testnet3, testnet4, regtest")
	dataDir := flag.String("data-dir", "data", "Data directory for config and storage")
	port := flag.Int("port", 0, "HTTP port override (0 = use config file value)")
	sysconfigPath := flag.String("sysconfig", "", "Path to JSON file with system config overrides")
	chainAPI := flag.String("chain-api", "", "Comma-separated Esplora/mempool.space API base URLs, tried in order: one that does not answer (network error, HTTP 5xx, 429) fails over to the next (default: the chain's public mempool.space)")

	flag.Parse()

	return args{
		isInit:        *isInit,
		debug:         *debug,
		network:       *network,
		chainName:     *chainName,
		chainNetwork:  *chainNetwork,
		dataDir:       *dataDir,
		port:          *port,
		sysconfigPath: *sysconfigPath,
		chainAPI:      *chainAPI,
	}, nil
}
