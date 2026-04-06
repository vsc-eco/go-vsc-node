package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	network       string
	dataDir       string
	sysconfigPath string
}

func ParseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("Create and broadcast a genesis election, usually for a testnet.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	network := flag.String("network", "testnet", "Network to create genesis election for")
	dataDir := flag.String("data-dir", "data", "Data directory for config and storage")
	sysconfigPath := flag.String("sysconfig", "", "Path to JSON file with system config overrides")

	flag.Parse()

	return args{
		*network,
		*dataDir,
		*sysconfigPath,
	}, nil
}
