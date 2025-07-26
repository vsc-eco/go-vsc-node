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
	flag.Parse()

	return args{
		*network,
		*wasmPath,
		*name,
		*desc,
	}, nil
}
