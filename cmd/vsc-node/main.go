package main

import (
	"fmt"
	"os"
	"time"

	cbortypes "vsc-node/lib/cbor-types"
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
	"vsc-node/modules/oracle"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	data_availability "vsc-node/modules/data-availability/server"
	"vsc-node/modules/vstream"
	wasm_parent_ipc "vsc-node/modules/wasm/parent_ipc"

	"github.com/vsc-eco/hivego"
)

func main() {
	cbortypes.RegisterTypes()
	init := os.Args[len(os.Args)-1] == "--init"
	dbConf := db.NewDbConfig()
	hiveApiUrl := streamer.NewHiveConfig()
	hiveApiUrlErr := hiveApiUrl.Init()

	fmt.Println("MONGO_URL", os.Getenv("MONGO_URL"))
	fmt.Println("HIVE_API", hiveApiUrl.Get().HiveURI)
	fmt.Println("Git Commit", announcements.GitCommit)

	dbImpl := db.New(dbConf)
	vscDb := vsc.New(dbImpl)
	reindexDb := db.NewReindex(vscDb.DbInstance)
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
	} else if hiveApiUrlErr != nil {
		fmt.Println("Failed to parse Hive API config", hiveApiUrlErr)
		os.Exit(1)
	}

	// choose the source
	hiveRpcClient := hivego.NewHiveRpc(hiveApiUrl.Get().HiveURI)

	filters := []streamer.FilterFunc{filter}
	//Default filter don't filter anything
	vFilters := []streamer.VirtualFilterFunc{
		func(op hivego.VirtualOp) bool {
			return op.Op.Type == "interest_operation"
		},
	}

	stBlock := uint64(94601000)
	streamerPlugin := streamer.NewStreamer(hiveRpcClient, hiveBlocks, filters, vFilters, &stBlock) // optional starting block #

	identityConfig := common.NewIdentityConfig()

	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  hiveRpcClient,
			KeyPair: identityConfig.HiveActiveKeyPair,
		},
	}

	sysConfig := common.SystemConfig{
		Network: "mainnet",
	}

	p2p := p2pInterface.New(witnessesDb, identityConfig, sysConfig)

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
	se := stateEngine.New(l, da, witnessDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionsDb, rcDb, nonceDb, wasm)

	rcSystem := se.RcSystem

	vstream := vstream.New(se)
	ep := election_proposer.New(p2p, witnessesDb, electionDb, balanceDb, da, &hiveCreator, identityConfig, se, vstream)

	bp := blockproducer.New(l, p2p, vstream, se, identityConfig, &hiveCreator, da, electionDb, vscBlocks, txDb, rcSystem, nonceDb)
	oracle := oracle.New(p2p, identityConfig, electionDb)

	multisig := gateway.New(l, witnessesDb, electionDb, actionsDb, balanceDb, &hiveCreator, vstream, p2p, se, identityConfig, hiveRpcClient)

	txpool := transactionpool.New(p2p, txDb, nonceDb, hiveBlocks, da, identityConfig, rcSystem)

	sr := streamer.NewStreamReader(hiveBlocks, vstream.ProcessBlock, se.SaveBlockHeight, stBlock)

	gqlManager := gql.New(gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: &gqlgen.Resolver{
		witnessDb,
		txpool,
		balanceDb,
		ledgerDbImpl,
		actionsDb,
		electionDb,
		txDb,
		nonceDb,
		rcDb,
		hiveBlocks,
		se,
		da,
		contractDb,
		contractState,
	}}), "0.0.0.0:8080")

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		//Configuration init
		dbConf,
		identityConfig,

		//DB plugin initialization
		dbImpl,
		vscDb,
		reindexDb,
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
		rcDb,
		nonceDb,
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
		oracle,
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
