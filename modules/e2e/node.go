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
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
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
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"
	wasm_parent_ipc "vsc-node/modules/wasm/parent_ipc"

	"vsc-node/modules/vstream"

	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
)

type Node struct {
	Aggregate        *aggregate.Aggregate
	StateEngine      *stateEngine.StateEngine
	P2P              *p2pInterface.P2PServer
	VStream          *vstream.VStream
	ElectionProposer election_proposer.ElectionProposer
	TxPool           *transactionpool.TransactionPool

	announcementsManager *announcements.AnnouncementsManager

	MockHiveBlocks *MockHiveDbs
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
	ledgerDbImpl := ledgerDb.New(vscDb)
	balanceDb := ledgerDb.NewBalances(vscDb)
	actionsDb := ledgerDb.NewActionsDb(vscDb)
	interestClaims := ledgerDb.NewInterestClaimDb(vscDb)
	contractState := contracts.NewContractState(vscDb)
	rcDb := rcDb.New(vscDb)
	nonceDb := nonces.New(vscDb)

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

	sysConfig := common.SystemConfig{
		Network: "mocknet",
	}

	p2p := p2pInterface.New(witnessesDb, identityConfig, sysConfig, 0)

	peerGetter := p2p.PeerInfo()

	announcementsManager, _ := announcements.New(hrpc, identityConfig, time.Hour*24, &txCreator, peerGetter)

	datalayer := DataLayer.New(p2p, input.Username)
	wasm := wasm_parent_ipc.New()

	se := stateEngine.New(logger, datalayer, witnessesDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionsDb, rcDb, nonceDb, wasm)

	txpool := transactionpool.New(p2p, txDb, nonceDb, hiveBlocks, datalayer, identityConfig, se.RcSystem)

	dbNuker := NewDbNuker(vscDb)

	vstream := vstream.New(se)

	ep := election_proposer.New(p2p, witnessesDb, electionDb, balanceDb, datalayer, &txCreator, identityConfig, se, vstream)

	bp := blockproducer.New(logger, p2p, vstream, se, identityConfig, &txCreator, datalayer, electionDb, vscBlocks, txDb, se.RcSystem, nonceDb)

	multisig := gateway.New(logger, witnessesDb, electionDb, actionsDb, balanceDb, &txCreator, vstream, p2p, se, identityConfig, hiveClient)

	plugins := make([]aggregate.Plugin, 0)

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
		ledgerDbImpl,
		actionsDb,
		balanceDb,
		interestClaims,
		contractState,
		rcDb,
		nonceDb,
		vstream,
		wasm,
		se,
		bp,
		ep,
		txpool,
		multisig,
	)

	if input.Primary {
		gqlManager := gql.New(gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: &gqlgen.Resolver{
			witnessesDb,
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
			datalayer,
			contractDb,
			contractState,
		}}), "0.0.0.0:7080")
		plugins = append(plugins, gqlManager)
	}

	if input.Runner != nil {

		fmt.Println("Setting r2e")
		input.Runner.Datalayer = datalayer
		input.Runner.Witnesses = witnessesDb
		input.Runner.HiveCreator = &txCreator

		input.Runner.ElectionProposer = ep
		input.Runner.VStream = vstream
	}

	return &Node{
		Aggregate:        aggregate.New(plugins),
		StateEngine:      se,
		P2P:              p2p,
		VStream:          vstream,
		ElectionProposer: ep,

		TxPool: txpool,

		announcementsManager: announcementsManager,

		MockHiveBlocks: hiveBlocks,
	}
}
