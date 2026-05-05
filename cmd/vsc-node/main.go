package main

import (
	"fmt"
	"os"
	"path"
	"time"

	cbortypes "vsc-node/lib/cbor-types"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/hive"
	"vsc-node/lib/vsclog"
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
	state_checkpoint "vsc-node/modules/db/vsc/state_checkpoint"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	election_proposer "vsc-node/modules/election-proposer"
	"vsc-node/modules/gateway"
	"vsc-node/modules/gql"
	"vsc-node/modules/gql/gqlgen"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	"vsc-node/modules/hive/streamer"
	"vsc-node/modules/oracle"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"
	"vsc-node/modules/tss"

	data_availability "vsc-node/modules/data-availability/server"
	wasm_runtime "vsc-node/modules/wasm/runtime_ipc"
	wasm_sdk "vsc-node/modules/wasm/sdk"

	flatfs "github.com/ipfs/go-ds-flatfs"
	"github.com/vsc-eco/hivego"
)

func main() {
	cbortypes.RegisterTypes()
	args, err := ParseArgs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error parsing arguments:", err)
		os.Exit(1)
	}
	initLogLevel(args.logLevel)

	log := vsclog.Module("magid")

	dbConf := db.NewDbConfig(args.dataDir)
	p2pConf := p2pInterface.NewConfig(args.dataDir)
	gqlConf := gql.NewGqlConfig(args.dataDir)
	oracleConf := oracle.NewOracleConfig(args.dataDir)
	hiveApiUrl := streamer.NewHiveConfig(args.dataDir)
	hiveApiUrlErr := hiveApiUrl.Init()

	hiveURIs := hiveApiUrl.Get().HiveURIs

	log.Info("starting magid", "network", args.network, "hive_nodes", hiveURIs, "git_commit", announcements.GitCommit)

	dbImpl := db.New(dbConf)
	vscDb := vsc.New(dbImpl, dbConf)
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
	checkpointDb := state_checkpoint.New(vscDb)
	sysConfig := systemconfig.FromNetwork(args.network)
	wasm_sdk.Init(sysConfig.OnMainnet())
	if args.sysconfigPath != "" {
		if args.network != "devnet" && args.network != "mocknet" {
			log.Error("sysconfig overrides only allowed on devnet/mocknet", "network", args.network)
			os.Exit(1)
		}
		if err := sysConfig.LoadOverrides(args.sysconfigPath); err != nil {
			log.Error("failed loading sysconfig overrides", "err", err)
			os.Exit(1)
		}
	}

	if err != nil {
		log.Error("startup error", "err", err)
		os.Exit(1)
	} else if hiveApiUrlErr != nil {
		log.Error("failed to parse Hive API config", "err", hiveApiUrlErr)
		os.Exit(1)
	}

	if sysConfig.OnMainnet() && args.disableTss {
		log.Error("cannot disable TSS plugin on mainnet")
		os.Exit(1)
	}

	// choose the source
	hiveRpcClient := hivego.NewHiveRpc(hiveURIs)
	hiveRpcClient.ChainID = sysConfig.HiveChainId()

	filters := []streamer.FilterFunc{filter}
	//Default filter don't filter anything
	vFilters := []streamer.VirtualFilterFunc{
		func(op hivego.VirtualOp) bool {
			return op.Op.Type == "interest_operation"
		},
	}

	stBlock := sysConfig.StartHeight()
	streamerPlugin := streamer.NewStreamer(hiveRpcClient, hiveBlocks, filters, vFilters, &stBlock) // optional starting block #

	identityConfig := common.NewIdentityConfig(args.dataDir)

	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  hiveRpcClient,
			KeyPair: identityConfig.HiveActiveKeyPair,
		},
	}

	//Set below from vstream
	var blockStatus common_types.BlockStatusGetter = nil
	p2p := p2pInterface.New(witnessesDb, p2pConf, identityConfig, sysConfig, blockStatus)

	announcementsManager, err := announcements.New(
		hiveRpcClient,
		identityConfig,
		sysConfig,
		p2pConf,
		time.Hour*24,
		&hiveCreator,
		p2p,
	)
	if err != nil {
		log.Error("announcements init failed", "err", err)
		os.Exit(1)
	}

	wasm := wasm_runtime.New()

	da := datalayer.New(p2p, args.dataDir)

	dataAvailability := data_availability.New(p2p, identityConfig, da)

	se := stateEngine.New(
		sysConfig,
		da,
		witnessDb,
		electionDb,
		contractDb,
		contractState,
		txDb,
		ledgerDbImpl,
		balanceDb,
		hiveBlocks,
		interestClaims,
		vscBlocks,
		actionsDb,
		rcDb,
		nonceDb,
		tssKeys,
		tssCommitments,
		tssRequests,
		wasm,
	).WithTxStarter(dbImpl).WithCheckpointDb(checkpointDb)

	rcSystem := se.RcSystem

	blockConsumer := blockconsumer.New(se)

	blockStatus = blockConsumer.BlockStatus()
	se.SetBlockStatus(blockStatus)
	ep := election_proposer.New(
		p2p,
		witnessesDb,
		electionDb,
		vscBlocks,
		balanceDb,
		da,
		&hiveCreator,
		identityConfig,
		sysConfig,
		se,
		blockConsumer,
	)

	bp := blockproducer.New(p2p, blockConsumer, se, identityConfig, sysConfig, &hiveCreator, da, electionDb, vscBlocks, txDb, rcSystem, nonceDb)

	txpool := transactionpool.New(p2p, txDb, nonceDb, electionDb, hiveBlocks, da, identityConfig, rcSystem)

	oracle := oracle.New(p2p, identityConfig, sysConfig, electionDb, witnessDb, blockConsumer, se, contractState, da, txpool, oracleConf, nonceDb)

	multisig := gateway.New(
		sysConfig,
		witnessesDb,
		electionDb,
		actionsDb,
		balanceDb,
		&hiveCreator,
		blockConsumer,
		p2p,
		se,
		identityConfig,
		hiveRpcClient,
	)

	sr := streamer.NewStreamReader(hiveBlocks, blockConsumer.ProcessBlock, se.SaveBlockHeight, stBlock)

	flatDb, err := flatfs.CreateOrOpen(path.Join(args.dataDir, "tss-keys"), flatfs.Prefix(1), false)
	if err != nil {
		panic(err)
	}

	tssMgr := tss.New(
		p2p,
		tssKeys,
		tssRequests,
		tssCommitments,
		witnessDb,
		electionDb,
		blockConsumer,
		se,
		identityConfig,
		sysConfig,
		flatDb,
		&hiveCreator,
	)

	gqlManager := gql.New(gqlgen.NewExecutableSchema(gqlgen.Config{Complexity: gql.NewComplexityRoot(), Resolvers: &gqlgen.Resolver{
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
		TssCommitments: tssCommitments,
		TssRequests:    tssRequests,
		InterestClaims: interestClaims,
		ChainOracle:    oracle.ChainOracle(),
	}}), gqlConf)

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		//Configuration init
		dbConf,
		p2pConf,
		identityConfig,
		gqlConf,
		oracleConf,

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
		checkpointDb,

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
	)

	if !args.disableTss {
		plugins = append(plugins, tssMgr)
	}

	//Setup graphql manager after everything is initialized
	plugins = append(plugins, gqlManager)

	a := aggregate.New(
		plugins,
	)

	if args.isInit {
		log.Info("initializing config")
		configs := aggregate.New([]aggregate.Plugin{
			dbConf,
			p2pConf,
			identityConfig,
			gqlConf,
			oracleConf,
			hiveApiUrl,
		})
		err = configs.Init()
	} else {
		err = a.Run()
	}
	if err != nil {
		log.Error("startup failure", "err", err)
		os.Exit(1)
	}
}
