package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	network     string
	wasmPath    string
	name        string
	description string
	isInit      bool
	gqlUrl      string
}

func ParseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("Deploy a WASM contract to VSC.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	network := flag.String("network", "mainnet", "Network to deploy contract to")
	wasmPath := flag.String("wasmPath", "main.wasm", "Path to compiled WASM bytecode")
	name := flag.String("name", "", "Name of the contract")
	desc := flag.String("description", "", "Description of the contract")
	isInit := flag.Bool("init", false, "Generate credentials config files")
	gqlUrl := flag.String("gqlUrl", "https://api.vsc.eco/api/v1/graphql", "GraphQL API URL for fetching latest election")
	flag.Parse()

	return args{
		*network,
		*wasmPath,
		*name,
		*desc,
		*isInit,
		*gqlUrl,
	}, nil
}
