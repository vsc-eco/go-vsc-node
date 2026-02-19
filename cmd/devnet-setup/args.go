package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	network    string
	dataDir    string
	nodes      int
	hiveUrl    string
	dbPrefix   string
	p2pPort    int
	witPrefix  string
	witCreator string
	wif        string
	stakeAmt   string
}

func ParseArgs() (args, error) {
	flag.Usage = func() {
		fmt.Printf("Initialize and bootstrap a Magi test network, usually a devnet.\n\n")
		fmt.Printf("Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	network := flag.String("network", "devnet", "Network to create genesis election for")
	dataDir := flag.String("data-dir", "data", "Parent data directory for config and storage for all nodes")
	nodes := flag.Int("nodes", 4, "Node count to initialize (min. 4)")
	hiveUrl := flag.String("hive-urls", "", "Hive RPC URLs (comma-separated)")
	dbPrefix := flag.String("db-prefix", "magi", "Database name prefix")
	p2pPort := flag.Int("p2p-port", 10720, "P2P port for the first node")
	witPrefix := flag.String("wit-prefix", "magi.test", "Witness username prefix")
	witCreator := flag.String("wit-creator", "initminer", "Username of witness account creator")
	wif := flag.String("wif", "", "Private active key of witness account creator")
	stakeAmt := flag.String("stake", "2000.000", "Stake amount for each witness")

	flag.Parse()

	return args{
		*network,
		*dataDir,
		*nodes,
		*hiveUrl,
		*dbPrefix,
		*p2pPort,
		*witPrefix,
		*witCreator,
		*wif,
		*stakeAmt,
	}, nil
}
