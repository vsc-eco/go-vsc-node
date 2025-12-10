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
	"vsc-node/modules/common/common_types"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	election_proposer "vsc-node/modules/election-proposer"
	"vsc-node/modules/gateway"
	"vsc-node/modules/gql"
	"vsc-node/modules/gql/gqlgen"
	"vsc-node/modules/gql/logstream"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	"vsc-node/modules/hive/streamer"
	"vsc-node/modules/oracle"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"
	"vsc-node/modules/tss"

	data_availability "vsc-node/modules/data-availability/server"
	wasm_runtime "vsc-node/modules/wasm/runtime_ipc"

	flatfs "github.com/ipfs/go-ds-flatfs"
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
	tssKeys := tss_db.NewKeys(vscDb)
	tssCommitments := tss_db.NewCommitments(vscDb)
	tssRequests := tss_db.NewRequests(vscDb)

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

	sysConfig := systemconfig.MainnetConfig()

	//Set below from vstream
	var blockStatus common_types.BlockStatusGetter = nil
	p2p := p2pInterface.New(witnessesDb, identityConfig, sysConfig, blockStatus)

	announcementsManager, err := announcements.New(hiveRpcClient, identityConfig, sysConfig, time.Hour*24, &hiveCreator, p2p)
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	wasm := wasm_runtime.New()

	da := datalayer.New(p2p)

	dataAvailability := data_availability.New(p2p, identityConfig, da)

	l := logger.PrefixedLogger{
		Prefix: "vsc-node",
	}
	ls := logstream.NewLogStream()
	se := stateEngine.New(l, sysConfig, da, witnessDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionsDb, rcDb, nonceDb, tssKeys, tssCommitments, tssRequests, wasm, ls)

	rcSystem := se.RcSystem

	blockConsumer := blockconsumer.New(se)

	blockStatus = blockConsumer.BlockStatus()
	ep := election_proposer.New(p2p, witnessesDb, electionDb, vscBlocks, balanceDb, da, &hiveCreator, identityConfig, sysConfig, se, blockConsumer)

	bp := blockproducer.New(l, p2p, blockConsumer, se, identityConfig, sysConfig, &hiveCreator, da, electionDb, vscBlocks, txDb, rcSystem, nonceDb)
	oracle := oracle.New(p2p, identityConfig, electionDb, witnessDb, blockConsumer, se)

	multisig := gateway.New(l, sysConfig, witnessesDb, electionDb, actionsDb, balanceDb, &hiveCreator, blockConsumer, p2p, se, identityConfig, hiveRpcClient)

	txpool := transactionpool.New(p2p, txDb, nonceDb, electionDb, hiveBlocks, da, identityConfig, rcSystem)

	sr := streamer.NewStreamReader(hiveBlocks, blockConsumer.ProcessBlock, se.SaveBlockHeight, stBlock)

	flatDb, err := flatfs.CreateOrOpen("data/tss-keys", flatfs.Prefix(1), false)
	if err != nil {
		panic(err)
	}

	tssMgr := tss.New(p2p, tssKeys, tssRequests, tssCommitments, witnessDb, electionDb, blockConsumer, se, identityConfig, flatDb, &hiveCreator)

	gqlManager := gql.New(gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: &gqlgen.Resolver{
		Witnesses:      witnessDb,
		TxPool:         txpool,
		Balances:       balanceDb,
		Ledger:         ledgerDbImpl,
		Actions:        actionsDb,
		Elections:      electionDb,
		Transactions:   txDb,
		Nonces:         nonceDb,
		Rc:             rcDb,
		HiveBlocks:     hiveBlocks,
		StateEngine:    se,
		Da:             da,
		Contracts:      contractDb,
		ContractsState: contractState,
		TssKeys:        tssKeys,
		TssRequests:    tssRequests,
		LogStream:      ls,
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
		tssKeys,
		tssCommitments,
		tssRequests,

		p2p,
		da,                   //Deps: [p2p]
		dataAvailability,     //Deps: [p2p]
		announcementsManager, // Deps: [p2p]

		blockConsumer,
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

		tssMgr,

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
