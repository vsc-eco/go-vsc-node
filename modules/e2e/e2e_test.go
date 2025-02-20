package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/announcements"
	blockproducer "vsc-node/modules/block-producer"
	"vsc-node/modules/common"
	"vsc-node/modules/config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/e2e"
	election_proposer "vsc-node/modules/election-proposer"
	"vsc-node/modules/vstream"

	cbortypes "vsc-node/lib/cbor-types"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/hive"
	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/vsc-eco/hivego"
)

//End to end test environment of VSC network

// 4 nodes minimum for 2/3 consensus minimum
const NODE_COUNT = 9

func TestE2E(t *testing.T) {
	config.UseMainConfigDuringTests = true
	cbortypes.RegisterTypes()
	mockReader := stateEngine.NewMockReader()

	mockCreator := stateEngine.MockCreator{
		Mr: mockReader,
	}

	broadcastFunc := func(tx hivego.HiveTransaction) error {
		insertOps := e2e.TransformTx(tx)

		txId, _ := tx.GenerateTrxId()

		mockCreator.BroadcastOps(insertOps, txId)

		return nil
	}
	runningNodes := make([]E2ENode, 0)

	//Make primary node

	r2e := &e2e.E2ERunner{
		BlockEvent: make(chan uint64),
	}

	doWithdraw := func() error {
		withdrawalRequest := map[string]interface{}{
			"net_id": common.NETWORK_ID,
			"amount": 1,
			"from":   "hive:test-account",
			"to":     "hive:test-account",
			"token":  "hbd",
		}

		serialzedBytes, _ := json.Marshal(withdrawalRequest)

		op := r2e.HiveCreator.CustomJson([]string{"test-account"}, []string{}, "vsc.withdraw", string(serialzedBytes))

		tx := r2e.HiveCreator.MakeTransaction([]hivego.HiveOperation{op})
		r2e.HiveCreator.PopulateSigningProps(&tx, nil)
		sig, _ := r2e.HiveCreator.Sign(tx)

		tx.AddSig(sig)

		r2e.HiveCreator.Broadcast(tx)

		tx.GenerateTrxId()

		return nil
	}

	r2e.SetSteps([]func() error{
		func() error {
			return nil
		},
		r2e.WaitToStart(), //This doesn't do anything right now
		func() error {
			return nil
		},
		r2e.Wait(5),
		r2e.BroadcastMockElection([]string{"e2e-0", "e2e-1", "e2e-2", "e2e-3"}),
		func() error {
			mockCreator.Transfer("test-account", "vsc.gateway", "50", "HBD", "test transfer")
			return nil
		},
		r2e.Wait(3),
		doWithdraw,
		doWithdraw,
		r2e.Wait(11),
		doWithdraw,
	})

	primaryNode := makeNode(t, "e2e-1", broadcastFunc, r2e)
	runningNodes = append(runningNodes, primaryNode)

	//Make the remaining 3 nodes for consensus operation
	for i := 2; i < NODE_COUNT+1; i++ {
		name := "e2e-" + strconv.Itoa(i)
		runningNodes = append(runningNodes, makeNode(t, name, broadcastFunc, nil))
	}

	plugs := make([]aggregate.Plugin, 0)

	for _, node := range runningNodes {
		plugs = append(plugs, node.Aggregate)
	}

	mainAggregate := aggregate.New(plugs)

	test_utils.RunPlugin(t, mainAggregate)

	mockReader.ProcessFunction = func(block hive_blocks.HiveBlock) {
		for _, node := range runningNodes {
			node.VStream.ProcessBlock(block)
		}
	}

	go func() {

		fmt.Println("Connecting local peers")
		time.Sleep(5 * time.Second)
		peerAddrs := make([]string, 0)

		for _, node := range runningNodes {
			for _, addr := range node.P2P.Host.Addrs() {
				peerAddrs = append(peerAddrs, addr.String()+"/p2p/"+node.P2P.Host.ID().String())
			}
		}

		for _, node := range runningNodes {
			for _, peerStr := range peerAddrs {
				peerId, _ := peer.AddrInfoFromString(peerStr)
				ctx := context.Background()
				ctx, _ = context.WithTimeout(ctx, 5*time.Second)
				node.P2P.Host.Connect(ctx, *peerId)
			}
		}
	}()

	mockReader.StartRealtime()

	// mockCreator.Transfer("test-account", "vsc.gateway", "10", "HBD", "test transfer")

	test_utils.RunPlugin(t, r2e, true)
}

// Mock seed for testing
const MOCK_SEED = "MOCK_SEED-"

func makeNode(t *testing.T, name string, mockBbrst func(tx hivego.HiveTransaction) error, r2e *e2e.E2ERunner) E2ENode {
	dbConf := db.NewDbConfig()
	db := db.New(dbConf)
	vscDb := vsc.New(db, name)
	hiveBlocks, _ := hive_blocks.New(vscDb)
	vscBlocks := vscBlocks.New(vscDb)
	witnessesDb := witnesses.New(vscDb)
	electionDb := elections.New(vscDb)
	contractDb := contracts.New(vscDb)
	txDb := transactions.New(vscDb)
	ledgerDbImpl := ledgerDb.New(vscDb)
	balanceDb := ledgerDb.NewBalances(vscDb)
	interestClaims := ledgerDb.NewInterestClaimDb(vscDb)
	contractState := contracts.NewContractState(vscDb)

	// hiveRpcClient := hivego.NewHiveRpc("https://api.hive.blog")
	identityConfig := common.NewIdentityConfig("data-" + name + "/config")

	identityConfig.Init()
	identityConfig.SetUsername(name)

	//Use different seeds so signatures come out differently.
	//It's recommended as multisig signing will by default filter out duplicate signatures
	kp := e2e.HashSeed([]byte(MOCK_SEED + name))

	brcst := hive.MockTransactionBroadcaster{
		KeyPair:  kp,
		Callback: mockBbrst,
	}

	txCreator := hive.MockTransactionCreator{
		MockTransactionBroadcaster: brcst,
		TransactionCrafter:         hive.TransactionCrafter{},
	}

	hrpc := &e2e.MockHiveRpcClient{}

	announcementsManager, err := announcements.New(hrpc, &identityConfig, time.Hour*24, &txCreator)

	go func() {
		//This should be executed via the e2e runner steps in the future
		//For now, we just wait a bit and then announce
		announcementsManager.Announce()
	}()
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	// setup := stateEngine.SetupEnv()

	p2p := p2pInterface.New(witnessesDb)

	datalayer := DataLayer.New(p2p, name)

	se := stateEngine.New(datalayer, witnessesDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks)

	dbNuker := e2e.NewDbNuker(vscDb)

	ep := election_proposer.New(p2p, witnessesDb, electionDb, datalayer, &txCreator, identityConfig)

	vstream := vstream.New(se)
	bp := blockproducer.New(p2p, vstream, se, &identityConfig, &txCreator, datalayer, electionDb)

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
		balanceDb,
		interestClaims,
		contractState,
		vstream,
		se,
		bp,
		ep,
	)

	if r2e != nil {
		r2e.Datalayer = datalayer
		r2e.Witnesses = witnessesDb
		r2e.HiveCreator = &txCreator

		r2e.ElectionProposer = ep
		r2e.VStream = vstream
		// plugins = append(plugins, r2e)
	}

	// go func() {
	// 	time.Sleep(15 * time.Second)
	// 	fmt.Println(p2p.Host.Addrs()[0], p2p.Host.ID())
	// 	for _, addr := range p2p.Host.Addrs() {
	// 		fmt.Println(addr.String() + "/p2p/" + p2p.Host.ID().String())
	// 	}

	return E2ENode{

		Aggregate:        aggregate.New(plugins),
		StateEngine:      se,
		P2P:              p2p,
		VStream:          vstream,
		ElectionProposer: ep,
	}
}

func cleanupNode() {

}

type E2ENode struct {
	Aggregate        *aggregate.Aggregate
	StateEngine      *stateEngine.StateEngine
	P2P              *p2pInterface.P2PServer
	VStream          *vstream.VStream
	ElectionProposer election_proposer.ElectionProposer
}
