package e2e_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/announcements"
	blockproducer "vsc-node/modules/block-producer"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/transactions"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/e2e"
	"vsc-node/modules/vstream"

	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/hive"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/vsc-eco/hivego"
)

//End to end test environment of VSC network

// 4 nodes minimum for 2/3 consensus minimum
const NODE_COUNT = 4

func TestE2E(t *testing.T) {
	mockReader := &stateEngine.MockReader{}

	mockCreator := stateEngine.MockCreator{
		Mr: mockReader,
	}

	broadcastFunc := func(tx hivego.HiveTransaction) error {
		insertOps := e2e.TransformTx(tx)
		mockCreator.BroadcastOps(insertOps)

		return nil
	}
	runningNodes := make([]E2ENode, 0)

	//Make primary node

	r2e := &e2e.E2ERunner{}

	r2e.SetSteps([]func(){
		r2e.WaitToStart(), //This doesn't do anything right now
		r2e.Sleep(5),
		r2e.BroadcastMockElection([]string{"e2e-1", "e2e-2", "e2e-3", "e2e-4"}),
		r2e.Produce(100),
	})

	primaryNode := makeNode("e2e-1", broadcastFunc, r2e)
	runningNodes = append(runningNodes, primaryNode)

	//Make the remaining 3 nodes for consensus operation
	for i := 2; i < NODE_COUNT+1; i++ {
		name := "e2e-" + strconv.Itoa(i)
		runningNodes = append(runningNodes, makeNode(name, broadcastFunc, nil))
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
	mockReader.ProcessFunction = func(block hive_blocks.HiveBlock) {
		for _, node := range runningNodes {
			node.StateEngine.ProcessBlock(block)
		}
	}

	mockReader.StartRealtime()

	mockCreator.Transfer("test-account", "vsc.gateway", "10", "HBD", "test transfer")

	select {}
}

// Mock seed for testing
const MOCK_SEED = "MOCK_SEED-"

func makeNode(name string, mockBbrst func(tx hivego.HiveTransaction) error, r2e *e2e.E2ERunner) E2ENode {
	dbConf := db.NewDbConfig()
	db := db.New(dbConf)
	vscDb := vsc.New(db, name)
	hiveBlocks, _ := hive_blocks.New(vscDb)
	witnessesDb := witnesses.New(vscDb)
	electionDb := elections.New(vscDb)
	contractDb := contracts.New(vscDb)
	txDb := transactions.New(vscDb)
	ledgerDbImpl := ledgerDb.New(vscDb)
	balanceDb := ledgerDb.NewBalances(vscDb)
	interestClaims := ledgerDb.NewInterestClaimDb(vscDb)
	contractState := contracts.NewContractState(vscDb)

	// hiveRpcClient := hivego.NewHiveRpc("https://api.hive.blog")
	announcementsConf := announcements.NewAnnouncementsConfig("data-" + name + "/config")

	announcementsConf.Init()
	announcementsConf.SetUsername(name)

	// wif := announcementsConf.Get().AnnouncementPrivateWif

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

	announcementsManager, err := announcements.New(hrpc, &announcementsConf, time.Hour*24, &txCreator)

	go func() {
		//This should be executed via the e2e runner steps in the future
		//For now, we just wait a bit and then announce
		time.Sleep(15 * time.Second)
		announcementsManager.Announce()
	}()
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	// setup := stateEngine.SetupEnv()

	p2p := p2pInterface.New(witnessesDb)

	datalayer := DataLayer.New(p2p, name)

	se := stateEngine.New(datalayer, witnessesDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims)

	dbNuker := e2e.NewDbNuker(vscDb)

	vstream := vstream.New()
	bp := blockproducer.New(vstream, se)

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		dbConf,
		db,
		announcementsConf,
		announcementsManager,
		vscDb,
		dbNuker,
		witnessesDb,
		p2p,
		datalayer,
		electionDb,
		contractDb,
		hiveBlocks,
		txDb,
		ledgerDbImpl,
		balanceDb,
		interestClaims,
		contractState,
		se,
		bp,
	)

	if r2e != nil {
		r2e.Datalayer = datalayer
		r2e.Witnesses = witnessesDb
		r2e.HiveCreator = &txCreator
		plugins = append(plugins, r2e)
	}

	go func() {
		a := aggregate.New(
			plugins,
		)

		err = a.Run()

		if err != nil {
			fmt.Println(err)
			fmt.Println("error is", err)
			os.Exit(1)
		}
	}()

	// go func() {
	// 	time.Sleep(15 * time.Second)
	// 	fmt.Println(p2p.Host.Addrs()[0], p2p.Host.ID())
	// 	for _, addr := range p2p.Host.Addrs() {
	// 		fmt.Println(addr.String() + "/p2p/" + p2p.Host.ID().String())
	// 	}
	// }()

	return E2ENode{
		StateEngine: se,
		P2P:         p2p,
	}
}

func cleanupNode() {

}

type E2ENode struct {
	StateEngine *stateEngine.StateEngine
	P2P         *p2pInterface.P2PServer
}
