package main

import (
	"fmt"
	"os"
	"os/signal"
	"path"
	"syscall"
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
	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	governance_db "vsc-node/modules/db/vsc/governance"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	"vsc-node/modules/db/vsc/pendulum_settlements"
	"vsc-node/modules/db/vsc/poaseats"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	election_proposer "vsc-node/modules/election-proposer"
	"vsc-node/modules/gateway"
	"vsc-node/modules/gql"
	"vsc-node/modules/gql/gqlgen"
	"vsc-node/modules/haf"
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

	if args.pprofAddr != "" {
		startPprofServer(args.pprofAddr)
	}

	// All node configuration lives in one aggregate so every config file is
	// loaded from disk (or seeded) exactly once, up front, before any code
	// below reads a value from it (Hive URIs, HAF URI, identity key, ...).
	dbConf := db.NewDbConfig(args.dataDir)
	p2pConf := p2pInterface.NewConfig(args.dataDir)
	gqlConf := gql.NewGqlConfig(args.dataDir)
	oracleConf := oracle.NewOracleConfig(args.dataDir)
	hiveApiUrl := streamer.NewHiveConfig(args.dataDir)
	identityConfig := common.NewIdentityConfig(args.dataDir)

	configs := aggregate.New([]aggregate.Plugin{
		dbConf,
		p2pConf,
		identityConfig,
		gqlConf,
		oracleConf,
		hiveApiUrl,
	})
	if err := configs.Init(); err != nil {
		log.Error("failed to initialize config", "err", err)
		os.Exit(1)
	}

	if args.isInit {
		log.Info("config initialized", "path", args.dataDir)
		return
	}

	hiveURIs := hiveApiUrl.Get().HiveURIs

	log.Info("starting magid", "network", args.network, "hive_nodes", hiveURIs, "git_commit", announcements.GitCommit)

	dbImpl := db.New(dbConf)
	vscDb := vsc.New(dbImpl, dbConf)
	hiveBlocks, err := hive_blocks.New(vscDb)
	witnessDb := witnesses.New(vscDb)
	vscBlocks := vscBlocks.New(vscDb)
	witnessesDb := witnesses.New(vscDb)
	electionDb := elections.New(vscDb)
	// Reindex gate: the usual REINDEX_ID/force path PLUS a consensus-version-lag
	// trigger — if the binary that last processed up to the head was BELOW the
	// chain-active version there (a version-adoption laggard that diverged locally),
	// and this binary can handle it, replay history from genesis under the correct
	// rules. ChainActiveAt reads the on-chain election active at the head.
	runningVer := consensusversion.RunningVersion()
	reindexDb := db.NewReindex(vscDb.DbInstance, args.forceReindex, &db.VersionReindex{
		RunningMajor:     runningVer.Major,
		RunningConsensus: runningVer.Consensus,
		// Query the elections collection directly off the (already-connected)
		// DbInstance rather than via electionDb.GetElectionByHeight: this callback
		// runs inside reindexDb.Init(), which the aggregate sequences BEFORE the
		// electionDb plugin Init that binds its mongo handle — using electionDb here
		// dereferences a nil collection and panics the node on restart.
		ChainActiveAt: func(blockHeight uint64) (uint64, uint64, bool) {
			v, ok := elections.ChainActiveVersionAt(vscDb.DbInstance, blockHeight)
			if !ok {
				return 0, 0, false
			}
			return v.Major, v.Consensus, true
		},
	})
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
	governanceDb := governance_db.New(vscDb)
	// POA seat registry — the on-chain allowlist of vetted operator accounts
	// eligible for election. Inert until the consensus floor reaches 0.7.0.
	poaSeatsDb := poaseats.New(vscDb)
	pendulumSettlementsDb := pendulum_settlements.New(vscDb)
	consensusStateDb := consensus_state.New(vscDb)
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
	// Block source: a non-empty HafDbURI in dbConfig switches block ingestion
	// to the local HAF database (hive.irreversible_*_view) and mongo keeps
	// only the graphql timestamp shims + cursor metadata. Otherwise the
	// classic Hive API streamer stores a filtered copy of every block.
	var blockSource hive_blocks.HiveBlocks = hiveBlocks
	var streamerPlugin aggregate.Plugin
	var hafSource *haf.Source
	if hafURI := dbConf.GetHafDbURI(); hafURI != "" {
		hafSource, err = haf.New(hafURI, hiveBlocks, filters, vFilters, stBlock)
		if err != nil {
			log.Error("failed to init HAF block source", "err", err)
			os.Exit(1)
		}
		blockSource = hafSource
		streamerPlugin = hafSource
		log.Info("block source", "source", "haf")
	} else {
		streamerPlugin = streamer.NewStreamer(hiveRpcClient, hiveBlocks, filters, vFilters, &stBlock) // optional starting block #
		log.Info("block source", "source", "hive_api")
	}

	// identityConfig was loaded from disk by the configs aggregate above, so the
	// on-disk Hive WIF is visible here (HasPrivateKey would otherwise only see
	// the in-memory "ADD_YOUR_PRIVATE_WIF" placeholder).
	hasPrivateKey := identityConfig.HasPrivateKey()

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
		blockSource,
		interestClaims,
		vscBlocks,
		actionsDb,
		rcDb,
		nonceDb,
		tssKeys,
		tssCommitments,
		tssRequests,
		governanceDb,
		poaSeatsDb,
		pendulumSettlementsDb,
		consensusStateDb,
		wasm,
		identityConfig,
	)

	rcSystem := se.RcSystem

	blockConsumer := blockconsumer.New(se)

	blockStatus = blockConsumer.BlockStatus()
	se.SetBlockStatus(blockStatus)
	ep := election_proposer.New(
		p2p,
		witnessesDb,
		electionDb,
		poaSeatsDb,
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

	txpool := transactionpool.New(p2p, txDb, nonceDb, electionDb, blockSource, da, identityConfig, rcSystem, se)

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

	sr := streamer.NewStreamReader(blockSource, blockConsumer.ProcessBlock, se.SaveBlockHeight, stBlock)

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
		contractState,
		da,
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
		HiveBlocks:     blockSource,
		StateEngine:    se,
		Da:             da,
		Contracts:      contractDb,
		ContractsState: contractState,
		TssKeys:        tssKeys,
		TssCommitments: tssCommitments,
		TssRequests:    tssRequests,
		Governance:     governanceDb,
		InterestClaims: interestClaims,
		ChainOracle:    oracle.ChainOracle(),
	}}), gqlConf)

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
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
		poaSeatsDb,
		interestClaims,
		contractState,
		tssKeys,
		tssCommitments,
		tssRequests,
		governanceDb,
		pendulumSettlementsDb,
		consensusStateDb,

		p2p,
		da,               //Deps: [p2p]
		dataAvailability, //Deps: [p2p]
		blockConsumer,
		//Startup main state processing pipeline
		streamerPlugin,
		se,
		sr,

		//WASM execution environment
		wasm,
		txpool,
	)

	if hasPrivateKey {
		plugins = append(plugins, announcementsManager, bp, oracle, ep, multisig, tssMgr)
	}

	//Setup graphql manager after everything is initialized
	plugins = append(plugins, gqlManager)

	a := aggregate.New(
		plugins,
	)

	// SIGINT/SIGTERM → graceful shutdown. Run() is the sole exit
	// authority: RequestShutdown flags the request and cancels the
	// aggregate ctx; Run's race-wait returns, Run stops every
	// plugin in reverse order and returns nil → main exits 0, so
	// supervisors see a clean stop, not a crash. A second signal
	// and a 30s timer are backstops against a hung shutdown (the
	// drain below is bounded at 10s; a clean teardown — gql's 5s
	// Shutdown cap plus the streamer's bounded store/stream/tracker
	// waits — normally completes in well under a second since every
	// loop observes the stop context immediately).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	go func() {
		sig := <-sigs
		log.Info("shutdown signal received; waiting for the current block to finish processing", "signal", sig.String())

		// Second signal forces an immediate exit.
		go func() {
			sig := <-sigs
			log.Error("second shutdown signal received; forcing immediate exit", "signal", sig.String())
			os.Exit(1)
		}()

		// Hung-teardown backstop.
		time.AfterFunc(30*time.Second, func() {
			log.Error("graceful shutdown timed out after 30s; forcing exit")
			os.Exit(1)
		})

		// Quiesce the block pipeline BEFORE any plugin teardown:
		// sr.Stop() cancels the block feed and then waits (bounded by
		// streamer.StopDrainTimeout) for the state engine to finish the
		// in-flight block — the listener's ProcessBlock runs to
		// completion, DB writes included — so plugins are never torn
		// down under a mid-block state write and the last-processed
		// cursor is checkpointed for a minimal-replay restart.
		sr.Stop()
		log.Info("block processing quiesced; proceeding with shutdown", "processedHeight", se.LastProcessedHeight())

		a.RequestShutdown()
	}()

	err = a.Run()
	if err != nil {
		log.Error("startup failure", "err", err)
		os.Exit(1)
	}
}
