package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	isInit     bool
	debug      bool
	network    string
	btcNetwork string
	dataDir    string
}

func parseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("VSC Mapping Bot - Monitors Bitcoin and submits mapping transactions.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	isInit := flag.Bool("init", false, "Initialize the bot (create config file and exit)")
	debug := flag.Bool("debug", false, "Enable debug logging")
	network := flag.String("network", "mainnet", "VSC network: mainnet, testnet, or devnet")
	btcNetwork := flag.String("btc-network", "mainnet", "Bitcoin network: mainnet, testnet4, testnet3, or regnet")
	dataDir := flag.String("data-dir", "data", "Data directory for config and storage")

	flag.Parse()

	return args{
		isInit:     *isInit,
		debug:      *debug,
		network:    *network,
		btcNetwork: *btcNetwork,
		dataDir:    *dataDir,
	}, nil
}
