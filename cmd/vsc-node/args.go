package main

import (
	"flag"
	"fmt"
	"os"

	"vsc-node/lib/vsclog"
)

type args struct {
	isInit  bool
	network string
	dataDir string

	disableTss    bool
	logLevel      string
	sysconfigPath string
}

func ParseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("Go VSC Node - A blockchain node for the Magi ecosystem.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	isInit := flag.Bool("init", false, "Initialize the node (run init instead of run)")
	network := flag.String("network", "mainnet", "Name of the network (mainnet or testnet)")
	dataDir := flag.String("data-dir", "data", "Data directory for config and storage")
	disableTss := flag.Bool("disable-tss", false, "Disable TSS plugin (testnet only)")
	logLevel := flag.String("log-level", "info", "Log level spec: error|warn|info|debug|verbose|trace or comma-separated with module overrides (e.g. error,tss=verbose,bp=info)")
	sysconfigPath := flag.String("sysconfig", "", "Path to JSON file with system config overrides")

	flag.Parse()

	return args{
		*isInit,
		*network,
		*dataDir,
		*disableTss,
		*logLevel,
		*sysconfigPath,
	}, nil
}

func initLogLevel(spec string) {
	vsclog.ParseAndApply(spec)
}
