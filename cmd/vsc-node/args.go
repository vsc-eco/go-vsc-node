package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	isInit   bool
	network  string
	dbSuffix string
}

func ParseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("Go VSC Node - A blockchain node for the Magi ecosystem.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	isInit := flag.Bool("init", false, "Initialize the node (run init instead of run)")
	network := flag.String("network", "mainnet", "Network to deploy contract to")
	dbSuffix := flag.String("db-suffix", "", "Optional database name suffix")

	flag.Parse()

	return args{
		*isInit,
		*network,
		*dbSuffix,
	}, nil
}
