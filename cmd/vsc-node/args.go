package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	isInit  bool
	network string
	gqlHost string
	dbName  string
	dataDir string
}

func ParseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("Go VSC Node - A blockchain node for the Magi ecosystem.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	isInit := flag.Bool("init", false, "Initialize the node (run init instead of run)")
	network := flag.String("network", "mainnet", "Network to deploy contract to")
	gqlHost := flag.String("gql-host", "0.0.0.0:8080", "GraphQL endpoint host")
	dbName := flag.String("db-name", "go-vsc", "Database name")
	dataDir := flag.String("data-dir", "data", "Data directory for config and storage")

	flag.Parse()

	return args{
		*isInit,
		*network,
		*gqlHost,
		*dbName,
		*dataDir,
	}, nil
}
