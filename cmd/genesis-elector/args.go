package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	isInit  bool
	network string
	dataDir string
}

func ParseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("Create and broadcast a genesis election, usually for a testnet.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	isInit := flag.Bool("init", false, "Initialize the node (run init instead of run)")
	network := flag.String("network", "testnet", "Network to create genesis election for")
	dataDir := flag.String("data-dir", "data", "Data directory for config and storage")

	flag.Parse()

	return args{
		*isInit,
		*network,
		*dataDir,
	}, nil
}
