package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	network        string
	dataDir        string
	nodes          int
	hiveUrl        string
	dbUrl          string
	dbPrefix       string
	dropDb         bool
	p2pHost        string
	p2pPort        int
	witPrefix      string
	witCreator     string
	wif            string
	stakeAmt       string
	sysconfigPath  string
	deployerName   string
	deployerHpAmt  string
	deployerHbdAmt string
	deployerOnly   bool
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
	dbUrl := flag.String("db-url", "mongodb://localhost:27017", "MongoDB database URL")
	dbPrefix := flag.String("db-prefix", "magi", "Database name prefix")
	dropDb := flag.Bool("drop-db", false, "Drop the database if exists")
	p2pHost := flag.String("p2p-host", "/ip4/127.0.0.1", "P2P bootstrap host")
	p2pPort := flag.Int("p2p-port", 10720, "P2P port for the first node")
	witPrefix := flag.String("wit-prefix", "magi.test", "Witness username prefix")
	witCreator := flag.String("wit-creator", "initminer", "Username of witness account creator")
	wif := flag.String("wif", "5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n", "Private active key of witness account creator")
	stakeAmt := flag.String("stake", "2000.000", "Stake amount for each witness")
	sysconfigPath := flag.String("sysconfig", "", "Path to JSON file with system config overrides")
	deployerName := flag.String("deployer-name", "vsc-deployer-1", "Hive account name for the contract deployer")
	deployerHpAmt := flag.String("deployer-hp", "10.000", "TESTS amount to vest as HP on the contract deployer (covers RC for broadcasts)")
	deployerHbdAmt := flag.String("deployer-hbd", "1000.000", "TBD amount to transfer to the contract deployer (covers per-deploy fees)")
	deployerOnly := flag.Bool("deployer-only", false, "Only create + fund the contract-deployer account. Loads existing witness identities to discover peer IDs for the deployer's bootnodes; skips witness/gateway/dao account creation and witness staking.")

	flag.Parse()

	return args{
		*network,
		*dataDir,
		*nodes,
		*hiveUrl,
		*dbUrl,
		*dbPrefix,
		*dropDb,
		*p2pHost,
		*p2pPort,
		*witPrefix,
		*witCreator,
		*wif,
		*stakeAmt,
		*sysconfigPath,
		*deployerName,
		*deployerHpAmt,
		*deployerHbdAmt,
		*deployerOnly,
	}, nil
}
