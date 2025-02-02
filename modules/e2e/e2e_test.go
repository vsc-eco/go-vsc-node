package e2e_test

import (
	"context"
	"crypto"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/announcements"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/transactions"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/e2e"

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
		// fmt.Println("Broadcast function is working", tx)

		// jbb, _ := json.Marshal(tx.Operations)

		var insertOps []hivego.Operation
		for _, op := range tx.Operations {
			opName := op.OpName()

			//Prepass, convert to flat map[string]interface{}
			var Value map[string]interface{}
			bval, _ := json.Marshal(op)
			json.Unmarshal(bval, &Value)

			// Do operation specific parsing to match hivego.Operation format
			if opName == "transfer" || opName == "transfer_from_savings" || opName == "transfer_to_savings" {
				rawAmount := Value["amount"].(string)

				splitAmt := strings.Split(rawAmount, " ")
				var nai string
				if splitAmt[1] == "HBD" {
					nai = "@@000000013"
				} else if splitAmt[1] == "HIVE" {
					nai = "@@000000021"
				}

				amtFloat, _ := strconv.ParseFloat(splitAmt[0], 64)

				//3 decimal places.
				amount := map[string]interface{}{
					"nai":       nai,
					"amount":    strconv.Itoa(int(amtFloat * 1000)),
					"precision": 3,
				}

				Value["amount"] = amount
			}
			//Probably not needed? Add more specific parsers as required

			insertOps = append(insertOps, hivego.Operation{
				Type:  opName,
				Value: Value,
			})
		}

		mockCreator.BroadcastOps(insertOps)

		return nil
	}

	runningNodes := make([]E2ENode, 0)
	for i := 0; i < NODE_COUNT; i++ {
		name := "e2e-" + strconv.Itoa(i)
		runningNodes = append(runningNodes, makeNode(name, broadcastFunc))
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

func hashSeed(seed []byte) *hivego.KeyPair {
	h := crypto.SHA256.New()
	h.Write(seed)
	hSeed := h.Sum(nil)
	return hivego.KeyPairFromBytes(hSeed)
}

func makeNode(name string, mockBbrst func(tx hivego.HiveTransaction) error) E2ENode {
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

	hiveRpcClient := hivego.NewHiveRpc("https://api.hive.blog")
	announcementsConf := announcements.NewAnnouncementsConfig()

	// wif := announcementsConf.Get().AnnouncementPrivateWif

	//Use different seeds so signatures come out differently.
	//It's recommended as multisig signing will by default filter out duplicate signatures
	kp := hashSeed([]byte(MOCK_SEED + name))

	brcst := hive.MockTransactionBroadcaster{
		KeyPair:  kp,
		Callback: mockBbrst,
		// {
		//
		// 	txId, _ := tx.GenerateTrxId()
		// 	return nil
		// },
	}

	txCreator := hive.MockTransactionCreator{
		MockTransactionBroadcaster: brcst,
		TransactionCrafter:         hive.TransactionCrafter{},
	}

	announcementsManager, err := announcements.New(hiveRpcClient, announcementsConf, time.Hour*24, &txCreator)

	go func() {
		// fmt.Println("Announceing after 15s")
		time.Sleep(15 * time.Second)
		// fmt.Println("Announceing NOW")
		announcementsManager.Announce()
	}()
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	// setup := stateEngine.SetupEnv()

	p2p := p2pInterface.New(witnessesDb)

	dl := DataLayer.New(p2p, name)

	se := stateEngine.New(dl, witnessesDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims)

	dbNuker := e2e.NewDbNuker(vscDb)

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
		electionDb,
		contractDb,
		hiveBlocks,
		txDb,
		ledgerDbImpl,
		balanceDb,
		interestClaims,
		contractState,
	)

	go func() {
		a := aggregate.New(
			plugins,
		)

		err = a.Run()

		if err != nil {
			fmt.Println("error is", err)
			os.Exit(1)
		}
	}()

	go func() {
		time.Sleep(15 * time.Second)
		fmt.Println(p2p.Host.Addrs()[0], p2p.Host.ID())
		for _, addr := range p2p.Host.Addrs() {
			fmt.Println(addr.String() + "/p2p/" + p2p.Host.ID().String())
		}
	}()

	return E2ENode{
		StateEngine: se,
		P2P:         p2p,
	}
}

func cleanupNode() {

}

type E2ENode struct {
	StateEngine stateEngine.StateEngine
	P2P         *p2pInterface.P2PServer
}
