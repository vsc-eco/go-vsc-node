package e2e_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strconv"
	"testing"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/config"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/e2e"
	transactionpool "vsc-node/modules/transaction-pool"

	cbortypes "vsc-node/lib/cbor-types"
	"vsc-node/lib/dids"
	"vsc-node/lib/test_utils"
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
	runningNodes := make([]e2e.Node, 0)

	//Make primary node

	r2e := &e2e.E2ERunner{
		BlockEvent: make(chan uint64),
	}

	doWithdraw := func() error {
		withdrawalRequest := transactionpool.VscWithdraw{
			Amount: "0.020",
			Asset:  "hbd",
			From:   "hive:test-account",
			To:     "hive:test-account",
			NetId:  "vsc-mainnet",
		}

		ops, _ := withdrawalRequest.SerializeHive()

		tx := r2e.HiveCreator.MakeTransaction(ops)
		r2e.HiveCreator.PopulateSigningProps(&tx, nil)
		sig, _ := r2e.HiveCreator.Sign(tx)

		tx.AddSig(sig)

		r2e.HiveCreator.Broadcast(tx)

		withdrawTxId, err := tx.GenerateTrxId()
		fmt.Println("[Prefix: e2e-1] Withdraw tx id", withdrawTxId, err)

		return nil
	}

	nodeNames := make([]string, 0)
	nodeNames = append(nodeNames, "e2e-1")
	for i := 2; i < NODE_COUNT+1; i++ {
		name := "e2e-" + strconv.Itoa(i)
		nodeNames = append(nodeNames, name)
	}

	primaryNode := e2e.MakeNode(e2e.MakeNodeInput{
		Username:  "e2e-1",
		BrcstFunc: broadcastFunc,
		Runner:    r2e,
	})
	runningNodes = append(runningNodes, *primaryNode)

	//Make the remaining 3 nodes for consensus operation
	for i := 2; i < NODE_COUNT+1; i++ {
		name := "e2e-" + strconv.Itoa(i)
		runningNodes = append(runningNodes, *e2e.MakeNode(e2e.MakeNodeInput{
			Username:  name,
			BrcstFunc: broadcastFunc,
			Runner:    nil,
		}))
	}

	plugs := make([]aggregate.Plugin, 0)

	for _, node := range runningNodes {
		plugs = append(plugs, node.Aggregate)
	}

	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	didKey, _ := dids.NewKeyDID(pubKey)

	transactionCreator := transactionpool.TransactionCrafter{
		Identity: dids.NewKeyProvider(privKey),
		Did:      didKey,

		VSCBroadcast: &transactionpool.InternalBroadcast{
			TxPool: runningNodes[0].TxPool,
		},
	}

	r2e.SetSteps([]func() error{
		r2e.WaitToStart(),
		func() error {
			return nil
		},
		r2e.Wait(10),
		r2e.BroadcastMockElection(nodeNames),
		func() error {
			fmt.Println("[Prefix: e2e-1] Executing test transfer from test-account to @vsc.gateway of 50 hbd")
			mockCreator.Transfer("test-account", "vsc.gateway", "50", "HBD", "to="+didKey.String())
			mockCreator.Transfer("test-account", "vsc.gateway", "50", "HBD", "")
			return nil
		},
		r2e.Wait(3),
		doWithdraw,
		doWithdraw,
		r2e.Wait(11),
		doWithdraw,

		func() error {
			for i := 0; i < 5; i++ {
				transferOp := &transactionpool.VSCTransfer{
					From:   didKey.String(),
					To:     "hive:vsc.account",
					Amount: "0.001",
					Asset:  "hbd",
					NetId:  "vsc-mainnet",
					Nonce:  uint64(i),
				}
				sTx, _ := transactionCreator.SignFinal(transferOp)

				transactionCreator.Broadcast(sTx)
			}

			stakeOp := &transactionpool.VSCStake{
				From:   didKey.String(),
				To:     didKey.String(),
				Amount: "0.010",
				Asset:  "hbd",
				NetId:  "vsc-mainnet",
				Type:   "stake",
				Nonce:  uint64(5),
			}
			sTx, err := transactionCreator.SignFinal(stakeOp)

			fmt.Println("Sign error", err, sTx)

			stakeId, err := transactionCreator.Broadcast(sTx)

			fmt.Println("stakeId", stakeId, err)
			return nil
		},
		r2e.Wait(40),
		func() error {
			fmt.Println("Preparing atomicId")
			withdrawTx := &transactionpool.VscWithdraw{
				From:   "hive:test-account",
				To:     "hive:test-account",
				Amount: "0.030",
				Asset:  "hbd",
				NetId:  "vsc-mainnet",
				Type:   "withdraw",
			}
			withdrawTx2 := &transactionpool.VscWithdraw{
				From:   "hive:test-account",
				To:     "hive:test-account",
				Amount: "0.060",
				Asset:  "hbd",
				NetId:  "vsc-mainnet",
				Type:   "withdraw",
			}

			ops, _ := withdrawTx.SerializeHive()
			ops2, _ := withdrawTx2.SerializeHive()
			totalOps := []hivego.HiveOperation{}
			totalOps = append(totalOps, ops...)
			totalOps = append(totalOps, ops2...)

			tx := r2e.HiveCreator.MakeTransaction(totalOps)

			r2e.HiveCreator.PopulateSigningProps(&tx, nil)
			sig, _ := r2e.HiveCreator.Sign(tx)

			tx.AddSig(sig)

			atomicId, _ := r2e.HiveCreator.Broadcast(tx)
			fmt.Println("atomicId", atomicId)

			return nil
		},

		// r2e.Produce(e2e.TimeToBlocks(time.Duration(time.Hour * 24 * 3))),
	})

	mainAggregate := aggregate.New(plugs)

	test_utils.RunPlugin(t, mainAggregate)

	plugsz := make([]aggregate.Plugin, 0)

	for _, node := range runningNodes {
		plugsz = append(plugsz, &node)
	}
	startupAggregate := aggregate.New(plugsz)

	test_utils.RunPlugin(t, startupAggregate, true)

	mockReader.ProcessFunction = func(block hive_blocks.HiveBlock) {
		for _, node := range runningNodes {
			node.VStream.ProcessBlock(block)
		}
	}

	func() {

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

	select {}
}

// Mock seed for testing
