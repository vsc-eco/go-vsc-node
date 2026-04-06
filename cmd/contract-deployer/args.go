package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	network       string
	wasmPath      string
	name          string
	description   string
	owner         string
	isInit        bool
	gqlUrl        string
	dataDir       string
	sysconfigPath string

	// update contract args
	contractId string
}

func ParseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("Deploy or update a WASM contract on Magi.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	network := flag.String("network", "mainnet", "Network to deploy contract to")
	wasmPath := flag.String("wasmPath", "", "Path to compiled WASM bytecode")
	name := flag.String("name", "", "Name of the contract")
	desc := flag.String("description", "", "Description of the contract")
	owner := flag.String("owner", "", "Owner of the contract (defaults to contract deployer)")
	isInit := flag.Bool("init", false, "Generate credentials config files")
	gqlUrl := flag.String("gqlUrl", "https://api.vsc.eco/api/v1/graphql", "GraphQL API URL for fetching latest election")
	dataDir := flag.String("data-dir", "data", "Data directory for config")
	contractId := flag.String("contractId", "", "Existing contract ID to update contract. Omit to deploy a new contract.")
	sysconfigPath := flag.String("sysconfig", "", "Path to JSON file with system config overrides")
	flag.Parse()

	return args{
		*network,
		*wasmPath,
		*name,
		*desc,
		*owner,
		*isInit,
		*gqlUrl,
		*dataDir,
		*sysconfigPath,
		*contractId,
	}, nil
}
