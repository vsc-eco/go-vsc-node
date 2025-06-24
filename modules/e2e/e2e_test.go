package e2e_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

	// "github.com/decred/dcrd/dcrec/secp256k1/v2"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/hasura/go-graphql-client"

	// secp256k1 "github.com/ethereum/go-ethereum/crypto/secp256k1"
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
		Primary:   true,
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

	// pubKey1, privKey1, _ := ed25519.GenerateKey(rand.Reader)
	// didKeyNoRcs, _ := dids.NewKeyDID(pubKey1)

	transactionCreator := transactionpool.TransactionCrafter{
		Identity: dids.NewKeyProvider(privKey),
		Did:      didKey,

		VSCBroadcast: &transactionpool.InternalBroadcast{
			TxPool: runningNodes[0].TxPool,
		},
	}

	// ethKeyHex := "ea3625737c9840af61e95a9fab172a5495b533978ba88cb68723514802119917" // 0x00000E1c8094cAC66CD1adf4C240cd9Cf43B4D46
	ethKeyHex := "5feac6ad3d3556a3a81bd9d2c881f195b5a8b4a5ce8f7bd4fa32c10bf186575a" // 0xcafe412dC5fb69FD5155a3b63A5AD6d3Bb80738b

	pk, _ := ethCrypto.GenerateKey()

	fmt.Println("eth key", hex.EncodeToString(ethCrypto.FromECDSA(pk)))
	privBytes, _ := hex.DecodeString(ethKeyHex)
	fmt.Println("privBytes", privBytes)
	ethPriv, err := ethCrypto.ToECDSA(privBytes)

	fmt.Println("ethPriv", ethPriv, err)

	ethProvider := dids.NewEthProvider(ethPriv)

	ethAddr := ethCrypto.PubkeyToAddress(ethPriv.PublicKey)
	ethDid := dids.NewEthDID(ethAddr.Hex())
	kk, _ := ethCrypto.ToECDSA(privBytes)

	fmt.Println("privBytes", hex.EncodeToString(privBytes))

	fmt.Println(ethCrypto.PubkeyToAddress(kk.PublicKey).Hex(), ethCrypto.PubkeyToAddress(ethPriv.PublicKey).Hex())

	ethCreator := transactionpool.TransactionCrafter{
		Identity: ethProvider,
		Did:      ethDid,

		VSCBroadcast: &transactionpool.InternalBroadcast{
			TxPool: runningNodes[0].TxPool,
		},
	}
	ethCreator.Did.String()

	// fmt.Println("EVM test")
	// fmt.Println("EVM test")
	// transferOp := &transactionpool.VSCTransfer{
	// 	From:   ethDid.String(),
	// 	To:     "hive:vsc.account",
	// 	Amount: "0.001",
	// 	Asset:  "hbd",
	// 	NetId:  "vsc-mainnet",
	// 	Nonce:  0,
	// }
	// transferOp2 := &transactionpool.VSCTransfer{
	// 	From:   ethDid.String(),
	// 	To:     "hive:vsc.account",
	// 	Amount: "0.001",
	// 	Asset:  "hbd",
	// 	NetId:  "vsc-mainnet",
	// 	Nonce:  0,
	// }
	// op, _ := transferOp.SerializeVSC()
	// op2, _ := transferOp2.SerializeVSC()
	// tx := transactionpool.VSCTransaction{
	// 	Ops: []transactionpool.VSCTransactionOp{
	// 		op,
	// 		op2,
	// 	},
	// }
	// sTx, err := ethCreator.SignFinal(tx)

	// bbb, _ := json.Marshal(sTx)
	// fmt.Println("VSCTransfer err", err, string(bbb))

	// transactionCreator.Broadcast(sTx)

	// transactionCreatorNoRc := transactionpool.TransactionCrafter{
	// 	Identity: dids.NewKeyProvider(privKey1),
	// 	Did:      didKeyNoRcs,

	// 	VSCBroadcast: &transactionpool.InternalBroadcast{
	// 		TxPool: runningNodes[0].TxPool,
	// 	},
	// }

	r2e.SetSteps([]func() error{
		r2e.WaitToStart(),
		func() error {
			return nil
		},
		r2e.Wait(10),
		// r2e.BroadcastMockElection(nodeNames),
		func() error {
			r2e.ElectionProposer.HoldElection(10)
			return nil
		},
		func() error {
			fmt.Println("[Prefix: e2e-1] Executing test transfer from test-account to @vsc.gateway of 50 hbd")
			mockCreator.Transfer("test-account", "vsc.gateway", "1500", "HBD", "to="+didKey.String())
			mockCreator.Transfer("test-account", "vsc.gateway", "1000", "HBD", "to="+ethDid.String())
			mockCreator.Transfer("test-account", "vsc.gateway", "50", "HBD", "")
			mockCreator.Transfer("test-account", "vsc.gateway", "50", "HBD", "to=0x25190d9443442765769Fe5CcBc8aA76151932a1A")
			mockCreator.Transfer("test-account", "vsc.gateway", "50", "HIVE", "")
			return nil
		},
		r2e.Wait(3),
		doWithdraw,
		doWithdraw,
		r2e.Wait(11),
		doWithdraw,
		func() error {
			transferOp := &transactionpool.VSCTransfer{
				From:   didKey.String(),
				To:     "hive:vsc.account",
				Amount: "0.001",
				Asset:  "hbd",
				NetId:  "vsc-mainnet",
			}
			op, _ := transferOp.SerializeVSC()
			tx := transactionpool.VSCTransaction{
				Ops: []transactionpool.VSCTransactionOp{
					op,
				},
			}
			sTx, err := transactionCreator.SignFinal(tx)

			bbb, _ := json.Marshal(sTx)
			fmt.Println("VSCTransfer err", err, string(bbb))

			transactionCreator.Broadcast(sTx)

			return nil
		},
		func() error {
			transferOp := &transactionpool.VSCTransfer{
				From:   ethDid.String(),
				To:     "hive:vsc.account",
				Amount: "0.001",
				Asset:  "hbd",
				NetId:  "vsc-mainnet",
			}
			op, _ := transferOp.SerializeVSC()
			tx := transactionpool.VSCTransaction{
				Ops: []transactionpool.VSCTransactionOp{
					op,
				},
			}
			sTx, err := ethCreator.SignFinal(tx)

			bbb, _ := json.Marshal(sTx)
			fmt.Println("VSCTransfer err", err, string(bbb))

			transactionCreator.Broadcast(sTx)

			return nil
		},
		func() error {
			stakeTx := &transactionpool.VscConsenusStake{
				Account: "hive:test-account",
				Amount:  "0.025",
				Type:    "stake",
				NetId:   "vsc-mainnet",
			}

			ops, err := stakeTx.SerializeHive()

			fmt.Println("consensus stake err", err)

			if err != nil {
				panic(err)
			}

			tx := r2e.HiveCreator.MakeTransaction(ops)
			r2e.HiveCreator.PopulateSigningProps(&tx, nil)
			sig, _ := r2e.HiveCreator.Sign(tx)
			tx.AddSig(sig)
			unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
			fmt.Println("stakeId", unstakeId)
			return nil
		},

		// func() error {
		// 	for i := 0; i < 5; i++ {
		// 		transferOp := &transactionpool.VSCTransfer{
		// 			From:   didKey.String(),
		// 			To:     "hive:vsc.account",
		// 			Amount: "0.001",
		// 			Asset:  "hbd",
		// 			NetId:  "vsc-mainnet",
		// 			Nonce:  uint64(i),
		// 		}
		// 		sTx, _ := transactionCreator.SignFinal(transferOp)

		// 		transactionCreator.Broadcast(sTx)
		// 	}

		// 	stakeOp := &transactionpool.VSCStake{
		// 		From:   didKey.String(),
		// 		To:     didKey.String(),
		// 		Amount: "0.020",
		// 		Asset:  "hbd",
		// 		NetId:  "vsc-mainnet",
		// 		Type:   "stake",
		// 		Nonce:  uint64(5),
		// 	}
		// 	sTx, err := transactionCreator.SignFinal(stakeOp)

		// 	fmt.Println("Sign error", err, sTx)

		// 	stakeId, err := transactionCreator.Broadcast(sTx)

		// 	fmt.Println("stakeId", stakeId, err)
		// 	return nil
		// },
		r2e.Wait(40),
		func() error {
			stakeTx := &transactionpool.VscConsenusStake{
				Account: "hive:test-account",
				Amount:  "0.025",
				Type:    "unstake",
				NetId:   "vsc-mainnet",
			}

			ops, err := stakeTx.SerializeHive()

			fmt.Println("consensus stake erro", err)
			if err != nil {
				panic(err)
			}

			tx := r2e.HiveCreator.MakeTransaction(ops)
			r2e.HiveCreator.PopulateSigningProps(&tx, nil)
			sig, _ := r2e.HiveCreator.Sign(tx)
			tx.AddSig(sig)

			unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
			fmt.Println("stakeId", unstakeId)
			return nil
		},
		// func() error {
		// 	transferTx := &transactionpool.VSCTransfer{
		// 		From:   didKey.String(),
		// 		To:     "hive:vsc.account",
		// 		Amount: "0.015",
		// 		Asset:  "hbd_savings",
		// 		NetId:  "vsc-mainnet",
		// 		Nonce:  uint64(6),
		// 	}
		// 	sTx, _ := transactionCreator.SignFinal(transferTx)

		// 	stakeId, err := transactionCreator.Broadcast(sTx)

		// 	fmt.Println("transferStakeId", stakeId, err)
		// 	return nil
		// },
		r2e.Wait(10),
		func() error {
			unstakeTx := &transactionpool.VSCStake{
				From:   "hive:vsc.account",
				To:     "hive:vsc.account",
				Amount: "0.001",
				Asset:  "hbd_savings",
				Type:   "unstake",
				NetId:  "vsc-mainnet",
			}

			ops, err := unstakeTx.SerializeHive()

			if err != nil {

				panic(err)
			}

			fmt.Println("Prefix: e2e-1] Unstake ops", ops, err)
			tx := r2e.HiveCreator.MakeTransaction(ops)
			r2e.HiveCreator.PopulateSigningProps(&tx, nil)
			sig, _ := r2e.HiveCreator.Sign(tx)
			tx.AddSig(sig)
			unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
			fmt.Println("unstakeId", unstakeId)

			return nil
		},
		r2e.Wait(20),
		func() error {
			unstakeTx := &transactionpool.VSCStake{
				From:   "hive:vsc.account",
				To:     "hive:vsc.account",
				Amount: "0.001",
				Asset:  "hbd_savings",
				Type:   "unstake",
				NetId:  "vsc-mainnet",
			}

			ops, err := unstakeTx.SerializeHive()

			if err != nil {

				panic(err)
			}

			fmt.Println("Prefix: e2e-1] Unstake ops", ops, err)
			tx := r2e.HiveCreator.MakeTransaction(ops)
			r2e.HiveCreator.PopulateSigningProps(&tx, nil)
			sig, _ := r2e.HiveCreator.Sign(tx)
			tx.AddSig(sig)
			unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
			fmt.Println("unstakeId", unstakeId)

			return nil
		},
		r2e.Wait(40),
		func() error {
			// mockCreator.Mr.MineNullBlocks(200)
			return nil
		},

		func() error {
			unstakeTx := &transactionpool.VSCStake{
				From:   "hive:vsc.account",
				To:     "hive:vsc.account",
				Amount: "0.001",
				Asset:  "hbd_savings",
				Type:   "unstake",
				NetId:  "vsc-mainnet",
			}

			ops, err := unstakeTx.SerializeHive()

			if err != nil {

				panic(err)
			}

			fmt.Println("Prefix: e2e-1] Unstake ops", ops, err)
			tx := r2e.HiveCreator.MakeTransaction(ops)
			r2e.HiveCreator.PopulateSigningProps(&tx, nil)
			sig, _ := r2e.HiveCreator.Sign(tx)
			tx.AddSig(sig)
			unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
			fmt.Println("unstakeId", unstakeId)

			return nil
		},
		r2e.Wait(20),
		func() error {
			mockCreator.ClaimInterest("vsc.gateway", 100)
			return nil
		},

		r2e.Wait(20),
		func() error {

			fmt.Println("syncBalance depositing")

			var demoAccounts = []string{
				"vsc.staker1",
				"vsc.staker2",
				"vsc.staker3",
				"vsc.staker4",
				"vsc.staker5",
				"vsc.staker6",
			}

			for _, account := range demoAccounts {
				mockCreator.Transfer(account, "vsc.gateway", "100", "HBD", "")
			}

			return nil
		},
		r2e.Wait(90),
		func() error {
			mockCreator.ClaimInterest("vsc.gateway", 50)
			return nil
		},
		// func() error {
		// 	fmt.Println("Preparing atomicId")
		// 	withdrawTx := &transactionpool.VscWithdraw{
		// 		From:   "hive:test-account",
		// 		To:     "hive:test-account",
		// 		Amount: "0.030",
		// 		Asset:  "hbd",
		// 		NetId:  "vsc-mainnet",
		// 		Type:   "withdraw",
		// 	}
		// 	withdrawTx2 := &transactionpool.VscWithdraw{
		// 		From:   "hive:test-account",
		// 		To:     "hive:test-account",
		// 		Amount: "0.060",
		// 		Asset:  "hbd",
		// 		NetId:  "vsc-mainnet",
		// 		Type:   "withdraw",
		// 	}

		// 	ops, _ := withdrawTx.SerializeHive()
		// 	ops2, _ := withdrawTx2.SerializeHive()
		// 	totalOps := []hivego.HiveOperation{}
		// 	totalOps = append(totalOps, ops...)
		// 	totalOps = append(totalOps, ops2...)

		// 	tx := r2e.HiveCreator.MakeTransaction(totalOps)

		// 	r2e.HiveCreator.PopulateSigningProps(&tx, nil)
		// 	sig, _ := r2e.HiveCreator.Sign(tx)

		// 	tx.AddSig(sig)

		// 	atomicId, _ := r2e.HiveCreator.Broadcast(tx)
		// 	fmt.Println("atomicId", atomicId)

		// 	return nil
		// },

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

	mockReader.ProcessFunction = func(block hive_blocks.HiveBlock, headHeight *uint64) {
		for _, node := range runningNodes {
			node.MockHiveBlocks.HighestBlock = block.BlockNumber
		}
		for _, node := range runningNodes {
			node.VStream.ProcessBlock(block, headHeight)
		}
	}

	func() {

		peerAddrs := make([]string, 0)

		for _, node := range runningNodes {
			for _, addr := range node.P2P.Addrs() {
				peerAddrs = append(peerAddrs, addr.String()+"/p2p/"+node.P2P.ID().String())
			}
		}

		for _, node := range runningNodes {
			for _, peerStr := range peerAddrs {
				peerId, _ := peer.AddrInfoFromString(peerStr)
				ctx := context.Background()
				ctx, _ = context.WithTimeout(ctx, 5*time.Second)
				fmt.Println("Trying to connect", peerId)
				node.P2P.Connect(ctx, *peerId)
			}
		}
	}()

	mockReader.StartRealtime()

	// mockCreator.Transfer("test-account", "vsc.gateway", "10", "HBD", "test transfer")

	test_utils.RunPlugin(t, r2e, true)

	select {}
}

// Mock seed for testing

func TestPostEVM(t *testing.T) {
	// ethKeyHex := "ea3625737c9840af61e95a9fab172a5495b533978ba88cb68723514802119917" // 0x00000E1c8094cAC66CD1adf4C240cd9Cf43B4D46
	ethKeyHex := "5feac6ad3d3556a3a81bd9d2c881f195b5a8b4a5ce8f7bd4fa32c10bf186575a" // 0xcafe412dC5fb69FD5155a3b63A5AD6d3Bb80738b

	pk, _ := ethCrypto.GenerateKey()

	fmt.Println("eth key", hex.EncodeToString(ethCrypto.FromECDSA(pk)))
	privBytes, _ := hex.DecodeString(ethKeyHex)
	fmt.Println("privBytes", privBytes)
	ethPriv, err := ethCrypto.ToECDSA(privBytes)

	fmt.Println("ethPriv", ethPriv, err)

	ethProvider := dids.NewEthProvider(ethPriv)

	ethAddr := ethCrypto.PubkeyToAddress(ethPriv.PublicKey)
	ethDid := dids.NewEthDID(ethAddr.Hex())
	kk, _ := ethCrypto.ToECDSA(privBytes)

	fmt.Println("privBytes", hex.EncodeToString(privBytes))

	fmt.Println(ethCrypto.PubkeyToAddress(kk.PublicKey).Hex(), ethCrypto.PubkeyToAddress(ethPriv.PublicKey).Hex())

	ethCreator := transactionpool.TransactionCrafter{
		Identity: ethProvider,
		Did:      ethDid,
	}

	gql := graphql.NewClient("http://localhost:7080/api/v1/graphql", nil)

	var nonceQuery struct {
		GetAccountNonce struct {
			Nonce graphql.Int `graphql:"nonce"`
		} `graphql:"getAccountNonce(account: $account)"`
	}

	err = gql.Query(context.Background(), &nonceQuery, map[string]any{
		"account": graphql.String(ethDid.String()),
	})

	if err != nil {
		fmt.Println("Error fetching nonce", err)
		return
	}

	nonce := int(nonceQuery.GetAccountNonce.Nonce)
	fmt.Println("Current nonce for", ethDid.String(), "is", nonce)

	transferOp := &transactionpool.VSCTransfer{
		From:   ethDid.String(),
		To:     "hive:vsc.account",
		Amount: "0.025",
		Asset:  "hbd",
		NetId:  "vsc-mainnet",
	}
	op, _ := transferOp.SerializeVSC()
	tx := transactionpool.VSCTransaction{
		Ops: []transactionpool.VSCTransactionOp{
			op,
		},
		Nonce: uint64(nonce),
	}
	sTx, err := ethCreator.SignFinal(tx)

	bbb, _ := json.Marshal(sTx)
	fmt.Println("VSCTransfer err", err, string(bbb))

	txStr := base64.StdEncoding.EncodeToString(sTx.Tx)
	sigStr := base64.StdEncoding.EncodeToString(sTx.Sig)

	var q struct {
		SubmitTransactionV1 struct {
			Id graphql.String `graphql:"id"`
		} `graphql:"submitTransactionV1(tx: $tx, sig: $sig)"`
	}

	err = gql.Query(context.Background(), &q, map[string]any{
		"tx":  graphql.String(txStr),
		"sig": graphql.String(sigStr),
	})

	fmt.Println("Transaction submitted", q, err)
}
