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
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	election_proposer "vsc-node/modules/election-proposer"
	"vsc-node/modules/gateway"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

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
}

func (n *Node) Init() error {
	return nil
}

func (n *Node) Start() *promise.Promise[any] {
	n.announcementsManager.Announce()
	return utils.PromiseResolve[any](nil)
}

func (n *Node) Stop() error {
	return nil
}

type MakeNodeInput struct {
	Username  string
	Runner    *E2ERunner
	BrcstFunc func(tx hivego.HiveTransaction) error
}

const SEED_PREFIX = "MOCK_SEED-"

func MakeNode(input MakeNodeInput) *Node {
	dbConf := db.NewDbConfig()
	db := db.New(dbConf)
	vscDb := vsc.New(db, input.Username)
	hiveBlocks, _ := hive_blocks.New(vscDb)
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
	logger := logger.PrefixedLogger{
		Prefix: input.Username,
	}

	identityConfig := common.NewIdentityConfig("data-" + input.Username + "/config")

	identityConfig.Init()
	identityConfig.SetUsername(input.Username)
	kp := HashSeed([]byte(SEED_PREFIX + input.Username))

	brcst := hive.MockTransactionBroadcaster{
		KeyPair:  kp,
		Callback: input.BrcstFunc,
	}

	txCreator := hive.MockTransactionCreator{
		MockTransactionBroadcaster: brcst,
		TransactionCrafter:         hive.TransactionCrafter{},
	}

	hrpc := &MockHiveRpcClient{}

	announcementsManager, _ := announcements.New(hrpc, identityConfig, time.Hour*24, &txCreator)

	p2p := p2pInterface.New(witnessesDb)

	datalayer := DataLayer.New(p2p, input.Username)
	txpool := transactionpool.New(p2p, txDb, datalayer, identityConfig)

	se := stateEngine.New(logger, datalayer, witnessesDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionsDb)

	dbNuker := NewDbNuker(vscDb)

	ep := election_proposer.New(p2p, witnessesDb, electionDb, datalayer, &txCreator, identityConfig)

	vstream := vstream.New(se)
	bp := blockproducer.New(logger, p2p, vstream, se, identityConfig, &txCreator, datalayer, electionDb, vscBlocks, txDb)

	multisig := gateway.New(logger, witnessesDb, electionDb, actionsDb, &txCreator, vstream, p2p, se, identityConfig)

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
		vstream,
		se,
		bp,
		ep,
		txpool,
		multisig,
	)

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
	}
}
