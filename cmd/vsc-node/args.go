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

	disableTss         bool
	forceReindex       bool
	logLevel           string
	sysconfigPath      string
	prefetchLookahead  uint64
	prefetchParallelism int
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
	forceReindex := flag.Bool("force-reindex", false, "Force a database reindex on startup (wipes derived state and replays from genesis)")
	logLevel := flag.String("log-level", "info", "Log level spec: error|warn|info|debug|verbose|trace or comma-separated with module overrides (e.g. error,tss=verbose,bp=info)")
	sysconfigPath := flag.String("sysconfig", "", "Path to JSON file with system config overrides")
	prefetchLookahead := flag.Uint64("prefetch-lookahead", 200, "How many hive blocks ahead of the state engine to scan for vsc.produce_block CIDs to prefetch into the IPFS blockstore.")
	prefetchParallelism := flag.Int("prefetch-parallelism", 8, "Number of concurrent IPFS fetch workers used by the block prefetcher. Set to 0 to disable prefetching.")

	flag.Parse()

	return args{
		*isInit,
		*network,
		*dataDir,
		*disableTss,
		*forceReindex,
		*logLevel,
		*sysconfigPath,
		*prefetchLookahead,
		*prefetchParallelism,
	}, nil
}

func initLogLevel(spec string) {
	vsclog.ParseAndApply(spec)
}
