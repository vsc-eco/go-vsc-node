package e2e

import (
	"fmt"
	"time"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/hive"
	"vsc-node/lib/logger"
	"vsc-node/lib/utils"
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
	ledger_db "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	rc_db "vsc-node/modules/db/vsc/rcs"
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
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"
	"vsc-node/modules/tss"

	data_availability "vsc-node/modules/data-availability/server"

	wasm_runtime "vsc-node/modules/wasm/runtime_ipc"

	"github.com/chebyrash/promise"
	flatfs "github.com/ipfs/go-ds-flatfs"
	"github.com/vsc-eco/hivego"
)

type Node struct {
	Aggregate *aggregate.Aggregate

	StateEngine      *stateEngine.StateEngine
	P2P              *p2pInterface.P2PServer
	HiveConsumer     *blockconsumer.HiveConsumer
	ElectionProposer election_proposer.ElectionProposer

	TxPool *transactionpool.TransactionPool

	announcementsManager *announcements.AnnouncementsManager

	MockHiveBlocks   *MockHiveDbs
	electionDb       elections.Elections
	contractsDb      contracts.Contracts
	balanceDb        ledger_db.Balances
	ledgerDb         ledger_db.Ledger
	interestClaimsDb ledger_db.InterestClaims
	nonceDb          nonces.Nonces
	rcDb             rc_db.RcDb
	transactionDb    transactions.Transactions
	tssCommitmentsDb tss_db.TssCommitments
	tssKeysDb        tss_db.TssKeys
	tssRequestsDb    tss_db.TssRequests
}

func (n *Node) Init() error {
	return nil
}

func (n *Node) Start() *promise.Promise[any] {
	// n.announcementsManager.Announce()
	return utils.PromiseResolve[any](nil)
}

func (n *Node) Stop() error {
	return nil
}

type MakeNodeInput struct {
	Username  string
	Runner    *E2ERunner
	BrcstFunc func(tx hivego.HiveTransaction) error
	Primary   bool
	Port      int
}

const SEED_PREFIX = "MOCK_SEED-"

func MakeNode(input MakeNodeInput) *Node {
	dbConf := db.NewDbConfig()
	db := db.New(dbConf)
	vscDb := vsc.New(db, input.Username)
	hiveBlocks := &MockHiveDbs{}
	vscBlocks := vscBlocks.New(vscDb)
	witnessesDb := witnesses.New(vscDb)
	electionDb := elections.New(vscDb)
	contractDb := contracts.New(vscDb)
	txDb := transactions.New(vscDb)
	ledgerDb := ledger_db.New(vscDb)
	balanceDb := ledger_db.NewBalances(vscDb)
	actionsDb := ledger_db.NewActionsDb(vscDb)
	interestClaims := ledger_db.NewInterestClaimDb(vscDb)
	contractState := contracts.NewContractState(vscDb)
	rcDb := rc_db.New(vscDb)
	nonceDb := nonces.New(vscDb)

	tssRequests := tss_db.NewRequests(vscDb)
	tssCommitments := tss_db.NewCommitments(vscDb)
	tssKeys := tss_db.NewKeys(vscDb)

	logger := logger.PrefixedLogger{
		Prefix: input.Username,
	}

	identityConfig := common.NewIdentityConfig("data-" + input.Username + "/config")

	identityConfig.Init()
	identityConfig.SetUsername(input.Username)
	kp := HashSeed([]byte(SEED_PREFIX + input.Username))

	hiveClient := hivego.NewHiveRpc("https://api.hive.blog")

	brcst := hive.MockTransactionBroadcaster{
		KeyPair:  kp,
		Callback: input.BrcstFunc,
	}

	txCreator := hive.MockTransactionCreator{
		MockTransactionBroadcaster: brcst,
		TransactionCrafter:         hive.TransactionCrafter{},
	}

	hrpc := &MockHiveRpcClient{}

	sysConfig := systemconfig.MocknetConfig()

	var blockStatus common_types.BlockStatusGetter = nil
	p2p := p2pInterface.New(witnessesDb, identityConfig, sysConfig, blockStatus, input.Port)

	announcementsManager, _ := announcements.New(hrpc, identityConfig, sysConfig, time.Hour*24, &txCreator, p2p)

	datalayer := DataLayer.New(p2p, input.Username)
	wasm := wasm_runtime.New()

	se := stateEngine.New(
		logger,
		sysConfig,
		datalayer,
		witnessesDb,
		electionDb,
		contractDb,
		contractState,
		txDb,
		ledgerDb,
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
	)

	blockConsumer := blockconsumer.New(se)

	txpool := transactionpool.New(p2p, txDb, nonceDb, electionDb, hiveBlocks, datalayer, identityConfig, se.RcSystem)

	dbNuker := NewDbNuker(vscDb)

	ep := election_proposer.New(p2p, witnessesDb, electionDb, vscBlocks, balanceDb, datalayer, &txCreator, identityConfig, sysConfig, se, blockConsumer)

	bp := blockproducer.New(logger, p2p, blockConsumer, se, identityConfig, sysConfig, &txCreator, datalayer, electionDb, vscBlocks, txDb, se.RcSystem, nonceDb)

	multisig := gateway.New(logger, sysConfig, witnessesDb, electionDb, actionsDb, balanceDb, &txCreator, blockConsumer, p2p, se, identityConfig, hiveClient)

	dataAvailability := data_availability.New(p2p, identityConfig, datalayer)

	sr := streamer.NewStreamReader(hiveBlocks, blockConsumer.ProcessBlock, se.SaveBlockHeight, 0)

	ds, err := flatfs.CreateOrOpen("data-"+input.Username+"/keys", flatfs.Prefix(1), false)
	if err != nil {
		panic(err)
	}
	tssManager := tss.New(p2p, tssKeys, tssRequests, tssCommitments, witnessesDb, electionDb, blockConsumer, se, identityConfig, ds, &txCreator)

	plugins := make([]aggregate.Plugin, 0)

	fmt.Println("dbNuke", dbNuker)
	plugins = append(plugins,
		dbConf,
		db,
		identityConfig,
		announcementsManager,
		vscDb,
		dbNuker,
		witnessesDb,
		p2p,
		datalayer,
		electionDb,
		contractDb,
		hiveBlocks,
		vscBlocks,
		txDb,
		ledgerDb,
		actionsDb,
		balanceDb,
		interestClaims,
		contractState,
		rcDb,
		nonceDb,
		tssCommitments,
		tssKeys,
		tssRequests,
		dataAvailability,
		blockConsumer,
		wasm,
		se,
		bp,
		ep,
		txpool,
		multisig,
		sr,
		tssManager,
	)

	if input.Primary {
		gqlManager := gql.New(gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: &gqlgen.Resolver{
			witnessesDb,
			txpool,
			balanceDb,
			ledgerDb,
			actionsDb,
			electionDb,
			txDb,
			nonceDb,
			rcDb,
			hiveBlocks,
			se,
			datalayer,
			contractDb,
			contractState,
			tssKeys,
			tssRequests,
		}}), "0.0.0.0:7080")
		plugins = append(plugins, gqlManager)
	}

	if input.Runner != nil {

		fmt.Println("Setting r2e")
		input.Runner.Datalayer = datalayer
		input.Runner.Witnesses = witnessesDb
		input.Runner.HiveCreator = &txCreator

		input.Runner.ElectionProposer = ep
		input.Runner.HiveConsumer = blockConsumer
		input.Runner.P2pService = p2p
		input.Runner.IdentityConfig = identityConfig
		input.Runner.SystemConfig = systemconfig.MocknetConfig()
		input.Runner.TxDb = txDb
	}

	return &Node{
		Aggregate:        aggregate.New(plugins),
		StateEngine:      se,
		P2P:              p2p,
		HiveConsumer:     blockConsumer,
		ElectionProposer: ep,

		TxPool: txpool,

		announcementsManager: announcementsManager,

		MockHiveBlocks:   hiveBlocks,
		electionDb:       electionDb,
		contractsDb:      contractDb,
		balanceDb:        balanceDb,
		ledgerDb:         ledgerDb,
		interestClaimsDb: interestClaims,
		nonceDb:          nonceDb,
		rcDb:             rcDb,
		transactionDb:    txDb,
		tssCommitmentsDb: tssCommitments,
		tssKeysDb:        tssKeys,
		tssRequestsDb:    tssRequests,
	}
}

type MakeClientInput struct {
	BrcstFunc func(tx hivego.HiveTransaction) error
}

type NodeClient struct {
	Plugins    []aggregate.Plugin
	P2PService *p2pInterface.P2PServer
	Identity   common.IdentityConfig
}

func MakeClient(input MakeClientInput) NodeClient {
	identityConfig := common.NewIdentityConfig("data-mock-client/config")

	identityConfig.Init()
	identityConfig.SetUsername("mock-client")

	sysConfig := systemconfig.MocknetConfig()
	wits := witnesses.NewEmptyWitnesses()
	p2p := p2pInterface.New(wits, identityConfig, sysConfig, nil, 0)

	plugins := make([]aggregate.Plugin, 0)
	plugins = append(plugins,
		identityConfig,
		p2p,
	)

	return NodeClient{
		Plugins:    plugins,
		P2PService: p2p,
		Identity:   identityConfig,
	}
}
