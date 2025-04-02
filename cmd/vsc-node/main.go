package main

import (
	"fmt"
	"os"
	"time"

	"vsc-node/lib/datalayer"
	"vsc-node/lib/hive"
	"vsc-node/lib/logger"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/announcements"
	blockproducer "vsc-node/modules/block-producer"
	"vsc-node/modules/common"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	election_proposer "vsc-node/modules/election-proposer"
	"vsc-node/modules/gateway"
	"vsc-node/modules/gql"
	"vsc-node/modules/gql/gqlgen"
	"vsc-node/modules/hive/streamer"
	p2pInterface "vsc-node/modules/p2p"
	rcSystem "vsc-node/modules/rc-system"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	data_availability "vsc-node/modules/data-availability"
	"vsc-node/modules/vstream"
	wasm_parent_ipc "vsc-node/modules/wasm/parent_ipc"

	"github.com/vsc-eco/hivego"
)

func main() {
	init := os.Args[len(os.Args)-1] == "--init"
	dbConf := db.NewDbConfig()
	hiveApiUrl := "https://api.hive.blog"

	fmt.Println("MONGO_URL", os.Getenv("MONGO_URL"))

	db := db.New(dbConf)
	vscDb := vsc.New(db)
	hiveBlocks, err := hive_blocks.New(vscDb)
	witnessDb := witnesses.New(vscDb)
	vscBlocks := vscBlocks.New(vscDb)
	witnessesDb := witnesses.New(vscDb)
	electionDb := elections.New(vscDb)
	contractDb := contracts.New(vscDb)
	txDb := transactions.New(vscDb)
	ledgerDbImpl := ledgerDb.New(vscDb)
	balanceDb := ledgerDb.NewBalances(vscDb)
	actionsDb := ledgerDb.NewActionsDb(vscDb)
	interestClaims := ledgerDb.NewInterestClaimDb(vscDb)
	contractState := contracts.NewContractState(vscDb)
	nonceDb := nonces.New(vscDb)
	rcDb := rcDb.New(vscDb)

	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	// set hive api url
	if hiveApiEnv, exists := os.LookupEnv("HIVE_API"); exists && hiveApiEnv != "" {
		hiveApiUrl = hiveApiEnv // Only override if non-empty
	}
	fmt.Println("HIVE_API", hiveApiUrl)

	// choose the source
	hiveRpcClient := hivego.NewHiveRpc("https://api.hive.blog")

	filters := []streamer.FilterFunc{filter}

	stBlock := uint64(94601000)
	streamerPlugin := streamer.NewStreamer(hiveRpcClient, hiveBlocks, filters, nil, &stBlock) // optional starting block #

	identityConfig := common.NewIdentityConfig()

	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  hiveRpcClient,
			KeyPair: identityConfig.HiveActiveKeyPair,
		},
	}

	p2p := p2pInterface.New(witnessesDb, identityConfig)

	peerGetter := p2p.PeerInfo()

	announcementsManager, err := announcements.New(hiveRpcClient, identityConfig, time.Hour*24, &hiveCreator, peerGetter)
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	wasm := wasm_parent_ipc.New() // TODO set proper cmd path

	da := datalayer.New(p2p)

	dataAvailability := data_availability.New(p2p, identityConfig, da)

	l := logger.PrefixedLogger{
		"vsc-node",
	}

	rcSystem := rcSystem.New(rcDb)

	se := stateEngine.New(l, da, witnessDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionsDb, rcDb, nonceDb, wasm)

	ep := election_proposer.New(p2p, witnessesDb, electionDb, balanceDb, da, &hiveCreator, identityConfig)

	vstream := vstream.New(se)
	bp := blockproducer.New(l, p2p, vstream, se, identityConfig, &hiveCreator, da, electionDb, vscBlocks, txDb, rcSystem, nonceDb)

	multisig := gateway.New(l, witnessesDb, electionDb, actionsDb, balanceDb, &hiveCreator, vstream, p2p, se, identityConfig, hiveRpcClient)

	txpool := transactionpool.New(p2p, txDb, da, identityConfig)

	sr := streamer.NewStreamReader(hiveBlocks, vstream.ProcessBlock, se.SaveBlockHeight, stBlock)

	gqlManager := gql.New(gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: &gqlgen.Resolver{
		witnessDb,
		txpool,
		balanceDb,
	}}), "localhost:8080")

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		//Configuration init
		dbConf,
		identityConfig,

		//DB plugin initialization
		db,
		vscDb,
		//DB collections
		witnessesDb,
		electionDb,
		witnessDb,
		contractDb,
		hiveBlocks,
		vscBlocks,
		txDb,
		ledgerDbImpl,
		actionsDb,
		balanceDb,
		interestClaims,
		contractState,

		p2p,
		da,                   //Deps: [p2p]
		dataAvailability,     //Deps: [p2p]
		announcementsManager, // Deps: [p2p]

		vstream,
		//Startup main state processing pipeline
		streamerPlugin,
		se,
		bp,
		ep,
		multisig,
		sr,

		//WASM execution environment
		wasm,
		txpool,

		//Setup graphql manager after everything is initialized
		gqlManager,
	)

	a := aggregate.New(
		plugins,
	)

	if init {
		fmt.Println("initing")
		err = a.Init()
	} else {
	err = a.Run()
	}
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}
}
