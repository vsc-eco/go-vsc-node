package e2e_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/transactions"
	wasm_runtime "vsc-node/modules/wasm/runtime"

	"vsc-node/modules/e2e"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	"vsc-node/lib/dids"

	// "github.com/decred/dcrd/dcrec/secp256k1/v2"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/hasura/go-graphql-client"
	"github.com/vsc-eco/hivego"
	// secp256k1 "github.com/ethereum/go-ethereum/crypto/secp256k1"
)

//go:embed artifacts/contract_test.wasm
var CONTRACT_WASM_OLD []byte

//go:embed artifacts/contract_test2.wasm
var CONTRACT_WASM []byte

//End to end test environment of VSC network

// 4 nodes minimum for 2/3 consensus minimum
const NODE_COUNT = 9

func TestE2E(t *testing.T) {
	config.UseMainConfigDuringTests = true

	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	didKey, _ := dids.NewKeyDID(pubKey)

	container := e2e.NewContainer(NODE_COUNT)

	container.Init()
	container.Start(t)

	transactionCreator := transactionpool.TransactionCrafter{
		Identity: dids.NewKeyProvider(privKey),
		Did:      didKey,

		VSCBroadcast: container.VSCBroadcast(),
	}

	pk, _ := ethCrypto.GenerateKey()

	fmt.Println("eth key", hex.EncodeToString(ethCrypto.FromECDSA(pk)))
	privBytes, _ := hex.DecodeString(e2e.EVM_KEY)
	fmt.Println("privBytes", privBytes)
	ethPriv, _ := ethCrypto.ToECDSA(privBytes)

	ethProvider := dids.NewEthProvider(ethPriv)

	ethAddr := ethCrypto.PubkeyToAddress(ethPriv.PublicKey)
	ethDid := dids.NewEthDID(ethAddr.Hex())
	// kk, _ := ethCrypto.ToECDSA(privBytes)

	// fmt.Println("privBytes", hex.EncodeToString(privBytes))

	// fmt.Println(ethCrypto.PubkeyToAddress(kk.PublicKey).Hex(), ethCrypto.PubkeyToAddress(ethPriv.PublicKey).Hex())

	ethCreator := transactionpool.TransactionCrafter{
		Identity: ethProvider,
		Did:      ethDid,

		VSCBroadcast: container.VSCBroadcast(),
	}

	fmt.Println("s", ethCreator)

	// fmt.Println("EVM test")
	// fmt.Println("EVM test")
	// transferOp := &transactionpool.VSCTransfer{
	// 	From:   ethDid.String(),
	// 	To:     "hive:vsc.account",
	// 	Amount: "0.001",
	// 	Asset:  "hbd",
	// 	NetId:  "vsc-mocknet",
	// 	Nonce:  0,
	// }
	// transferOp2 := &transactionpool.VSCTransfer{
	// 	From:   ethDid.String(),
	// 	To:     "hive:vsc.account",
	// 	Amount: "0.001",
	// 	Asset:  "hbd",
	// 	NetId:  "vsc-mocknet",
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

	graphClient := graphql.NewClient("http://localhost:7080/api/v1/graphql", nil)

	mockCreator := container.HiveCreator

	r2e := container.Runner()
	container.AddStep(r2e.WaitToStart())
	container.AddStep(r2e.Wait(10))
	container.AddStep(r2e.BroadcastElection())

	container.AddStep(e2e.Step{
		Name: "Hold election and transfer",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {

			mockCreator.Transfer("test-account", "vsc.gateway", "15000", "HBD", "to="+didKey.String())
			mockCreator.Transfer("test-account", "vsc.gateway", "1500", "HIVE", "to="+didKey.String())
			mockCreator.Transfer("test-account", "vsc.gateway", "10000", "HBD", "to="+ethDid.String())
			mockCreator.Transfer("test-account", "vsc.gateway", "1000", "HIVE", "to="+ethDid.String())
			//Balance goes to 0x25190d9443442765769Fe5CcBc8aA76151932a1A
			mockCreator.Transfer("test-account", "vsc.gateway", "50", "HBD", "to=0x25190d9443442765769Fe5CcBc8aA76151932a1A")
			//Balance goes to @test-account
			mockCreator.Transfer("test-account", "vsc.gateway", "50", "HBD", "")
			mockCreator.Transfer("test-account", "vsc.gateway", "50000", "HBD", "to=vaultec")
			mockCreator.Transfer("test-account", "vsc.gateway", "50", "HIVE", "")
			return func(ctx e2e.StepCtx) error {
				time.Sleep(40 * time.Second)
				var rcQuery struct {
					GetAccountRc struct {
						Amount graphql.Int `graphql:"amount"`
					} `graphql:"getAccountRC(account: $account)"`
					GetAccountBalance struct {
						Hbd  graphql.Int `graphql:"hbd"`
						Hive graphql.Int `graphql:"hive"`
					} `graphql:"getAccountBalance(account: $account)"`
				}
				graphClient.Query(context.Background(), &rcQuery, map[string]any{
					"account": graphql.String(didKey.String()),
				})

				fmt.Println("EVALUATE Account RC", rcQuery)
				return nil
			}, nil
		},
	})
	container.AddStep(r2e.Wait(10))

	var contractId string
	container.AddStep(e2e.Step{
		Name: "Deploy Contract",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			fmt.Println("Deploying contract...")
			storageProof, err := ctx.Container.Client().RequestProof("http://localhost:7080/api/v1/graphql", CONTRACT_WASM_OLD)

			if err != nil {
				return nil, err
			}

			fmt.Println("storageProof", storageProof)

			tx := stateEngine.TxCreateContract{
				Version:      "0.1",
				NetId:        "vsc-mocknet",
				Name:         "test-contract",
				Description:  "A test contract",
				Owner:        "hive:vaultec",
				Code:         storageProof.Hash,
				Runtime:      wasm_runtime.Go,
				StorageProof: storageProof,
			}

			j, err := json.Marshal(tx)

			transferOp := r2e.HiveCreator.Transfer("vaultec", "vsc.gateway", "10", "HBD", "contract_deployment")

			deployContract := r2e.HiveCreator.CustomJson([]string{"vaultec"}, []string{}, "vsc.create_contract", string(j))

			hiveTx := r2e.HiveCreator.MakeTransaction([]hivego.HiveOperation{
				deployContract,
				transferOp,
			})
			r2e.HiveCreator.PopulateSigningProps(&hiveTx, nil)

			txId, err := r2e.HiveCreator.Broadcast(hiveTx)

			fmt.Println("txId err", txId, err)

			contractId = common.ContractId(txId, 0)

			fmt.Println("ContractId Is:", contractId)
			return func(ctx e2e.StepCtx) error {
				return nil
			}, nil
		},
	})

	container.AddStep(r2e.DupElection(10 * time.Second))
	container.AddStep(e2e.Step{
		Name: "Update Contract",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			fmt.Println("Updating contract...")
			storageProof, err := ctx.Container.Client().RequestProof("http://localhost:7080/api/v1/graphql", CONTRACT_WASM)

			if err != nil {
				return nil, err
			}

			fmt.Println("storageProof", storageProof)

			tx := stateEngine.TxUpdateContract{
				NetId:        "vsc-mocknet",
				Id:           contractId,
				Name:         "test-contract",
				Description:  "A test contract being updated",
				Code:         storageProof.Hash,
				Runtime:      &wasm_runtime.Go,
				StorageProof: &storageProof,
			}

			j, err := json.Marshal(tx)

			transferOp := r2e.HiveCreator.Transfer("vaultec", "vsc.gateway", "10", "HBD", "contract_deployment")

			updateContract := r2e.HiveCreator.CustomJson([]string{"vaultec"}, []string{}, "vsc.update_contract", string(j))

			hiveTx := r2e.HiveCreator.MakeTransaction([]hivego.HiveOperation{
				updateContract,
				transferOp,
			})
			r2e.HiveCreator.PopulateSigningProps(&hiveTx, nil)
			txId, err := r2e.HiveCreator.Broadcast(hiveTx)

			fmt.Println("txId err", txId, err)
			return func(ctx e2e.StepCtx) error {
				return nil
			}, nil
		},
	})

	container.AddStep(r2e.Wait(20))
	container.AddStep(e2e.Step{
		Name: "Execute Contract - Test 1",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			statePut := &transactionpool.VscContractCall{
				Caller:     didKey.String(),
				ContractId: contractId,
				RcLimit:    200,
				Action:     "test1",
				Payload:    "test",
			}
			createTssKey := &transactionpool.VscContractCall{
				Caller:     didKey.String(),
				ContractId: contractId,
				RcLimit:    200,
				Action:     "createKey",
				Payload:    "test",
			}
			tokenDraw := &transactionpool.VscContractCall{
				Caller:     didKey.String(),
				ContractId: contractId,
				RcLimit:    200,
				Action:     "test3",
				Payload:    "test",
				Intents: []contracts.Intent{
					{
						Type: "transfer.allow",
						Args: map[string]string{
							"limit": "1.000",
							"token": "hive",
						},
					},
				},
			}
			op1, err := statePut.SerializeVSC()
			op2, err := createTssKey.SerializeVSC()
			op3, err := tokenDraw.SerializeVSC()

			if err != nil {
				return nil, err
			}
			tx := transactionpool.VSCTransaction{
				Ops:   []transactionpool.VSCTransactionOp{op1, op2, op3},
				Nonce: 0,
				NetId: "vsc-mocknet",
			}
			sTx, err := transactionCreator.SignFinal(tx)
			txId, err := transactionCreator.Broadcast(sTx)

			if err != nil {
				return nil, err
			}

			fmt.Println("txId", txId)
			return e2e.TxStatusAssertion([]e2e.TxStatusAssert{{txId, transactions.TransactionStatusConfirmed}}, 60), nil
		},
	})

	container.AddStep(e2e.Step{
		Name: "Update Contract Metadata",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			fmt.Println("Updating contract metadata...")

			tx := stateEngine.TxUpdateContract{
				NetId:       "vsc-mocknet",
				Id:          contractId,
				Name:        "New test contract",
				Description: "A test contract being updated again",
			}

			j, err := json.Marshal(tx)
			updateContract := r2e.HiveCreator.CustomJson([]string{"vaultec"}, []string{}, "vsc.update_contract", string(j))
			hiveTx := r2e.HiveCreator.MakeTransaction([]hivego.HiveOperation{updateContract})
			r2e.HiveCreator.PopulateSigningProps(&hiveTx, nil)
			txId, err := r2e.HiveCreator.Broadcast(hiveTx)

			fmt.Println("txId err", txId, err)
			return func(ctx e2e.StepCtx) error {
				return nil
			}, nil
		},
	})

	container.AddStep(r2e.DupElection(30 * time.Second))

	container.AddStep(e2e.Step{
		Name: "Execute Contract - Test 2",
		TestFunc: func(ctx e2e.StepCtx) (e2e.EvaluateFunc, error) {
			// Tx 1
			stateGetMod := &transactionpool.VscContractCall{
				Caller:     didKey.String(),
				ContractId: contractId,
				RcLimit:    200,
				Action:     "test2",
				Payload:    "test",
			}
			signTssKeyOp := &transactionpool.VscContractCall{
				Caller:     didKey.String(),
				ContractId: contractId,
				RcLimit:    200,
				Action:     "signKey",
				Payload:    "test",
			}
			tokenSend := &transactionpool.VscContractCall{
				Caller:     didKey.String(),
				ContractId: contractId,
				RcLimit:    200,
				Action:     "test4",
				Payload:    "test",
				Intents: []contracts.Intent{
					{
						Type: "transfer.allow",
						Args: map[string]string{
							"limit": "1.000",
							"token": "hive",
						},
					},
				},
			}
			op1, err := stateGetMod.SerializeVSC()
			op2, err := signTssKeyOp.SerializeVSC()
			op3, err := tokenSend.SerializeVSC()

			if err != nil {
				return nil, err
			}
			tx := transactionpool.VSCTransaction{
				Ops:   []transactionpool.VSCTransactionOp{op1, op2, op3},
				Nonce: 1,
				NetId: "vsc-mocknet",
			}
			sTx, err := transactionCreator.SignFinal(tx)
			txId, err := transactionCreator.Broadcast(sTx)

			if err != nil {
				return nil, err
			}

			fmt.Println("txId", txId)

			// Tx 2
			notExist := &transactionpool.VscContractCall{
				Caller:     didKey.String(),
				ContractId: contractId,
				RcLimit:    200,
				Action:     "does_not_exist",
				Payload:    "test",
				Intents:    []contracts.Intent{},
			}
			op4, err := notExist.SerializeVSC()

			if err != nil {
				return nil, err
			}
			tx2 := transactionpool.VSCTransaction{
				Ops: []transactionpool.VSCTransactionOp{
					op4,
				},
				Nonce: 2,
				NetId: "vsc-mocknet",
			}
			sTx2, _ := transactionCreator.SignFinal(tx2)
			txId2, err := transactionCreator.Broadcast(sTx2)

			if err != nil {
				return nil, err
			}

			fmt.Println("txId2", txId2)
			return e2e.TxStatusAssertion([]e2e.TxStatusAssert{{txId, transactions.TransactionStatusConfirmed}, {txId2, transactions.TransactionStatusFailed}}, 60), nil
		},
	})
	container.AddStep(r2e.Wait(30))

	err := container.RunSteps(t)

	if err != nil {
		return
	}

	container.Stop()

	// r2e.SetSteps([]func() error{
	// 	r2e.WaitToStart(),
	// 	func() error {
	// 		return nil
	// 	},
	// 	r2e.Wait(10),
	// 	// r2e.BroadcastMockElection(nodeNames),
	// 	func() error {
	// 		r2e.ElectionProposer.HoldElection(10)
	// 		return nil
	// 	},
	// 	func() error {
	// 		fmt.Println("[Prefix: e2e-1] Executing test transfer from test-account to @vsc.gateway of 50 hbd")
	// 		mockCreator.Transfer("test-account", "vsc.gateway", "1500", "HBD", "to="+didKey.String())
	// 		mockCreator.Transfer("test-account", "vsc.gateway", "1000", "HBD", "to="+ethDid.String())
	// 		mockCreator.Transfer("test-account", "vsc.gateway", "50", "HBD", "")
	// 		mockCreator.Transfer("test-account", "vsc.gateway", "50", "HBD", "to=0x25190d9443442765769Fe5CcBc8aA76151932a1A")
	// 		mockCreator.Transfer("test-account", "vsc.gateway", "50", "HIVE", "")
	// 		return nil
	// 	},
	// 	r2e.Wait(3),
	// 	doWithdraw,
	// 	doWithdraw,
	// 	r2e.Wait(11),
	// 	doWithdraw,
	// 	func() error {
	// 		transferOp := &transactionpool.VSCTransfer{
	// 			From:   didKey.String(),
	// 			To:     "hive:vsc.account",
	// 			Amount: "0.001",
	// 			Asset:  "hbd",
	// 			NetId:  "vsc-mocknet",
	// 		}
	// 		op, _ := transferOp.SerializeVSC()
	// 		tx := transactionpool.VSCTransaction{
	// 			Ops: []transactionpool.VSCTransactionOp{
	// 				op,
	// 			},
	// 		}
	// 		sTx, err := transactionCreator.SignFinal(tx)

	// 		bbb, _ := json.Marshal(sTx)
	// 		fmt.Println("VSCTransfer err", err, string(bbb))

	// 		transactionCreator.Broadcast(sTx)

	// 		return nil
	// 	},
	// 	func() error {
	// 		transferOp := &transactionpool.VSCTransfer{
	// 			From:   ethDid.String(),
	// 			To:     "hive:vsc.account",
	// 			Amount: "0.001",
	// 			Asset:  "hbd",
	// 			NetId:  "vsc-mocknet",
	// 		}
	// 		op, _ := transferOp.SerializeVSC()
	// 		tx := transactionpool.VSCTransaction{
	// 			Ops: []transactionpool.VSCTransactionOp{
	// 				op,
	// 			},
	// 		}
	// 		sTx, err := ethCreator.SignFinal(tx)

	// 		bbb, _ := json.Marshal(sTx)
	// 		fmt.Println("VSCTransfer err", err, string(bbb))

	// 		transactionCreator.Broadcast(sTx)

	// 		return nil
	// 	},
	// 	func() error {
	// 		stakeTx := &transactionpool.VscConsenusStake{
	// 			Account: "hive:test-account",
	// 			Amount:  "0.025",
	// 			Type:    "stake",
	// 			NetId:   "vsc-mocknet",
	// 		}

	// 		ops, err := stakeTx.SerializeHive()

	// 		fmt.Println("consensus stake err", err)

	// 		if err != nil {
	// 			panic(err)
	// 		}

	// 		tx := r2e.HiveCreator.MakeTransaction(ops)
	// 		r2e.HiveCreator.PopulateSigningProps(&tx, nil)
	// 		sig, _ := r2e.HiveCreator.Sign(tx)
	// 		tx.AddSig(sig)
	// 		unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
	// 		fmt.Println("stakeId", unstakeId)
	// 		return nil
	// 	},

	// 	// func() error {
	// 	// 	for i := 0; i < 5; i++ {
	// 	// 		transferOp := &transactionpool.VSCTransfer{
	// 	// 			From:   didKey.String(),
	// 	// 			To:     "hive:vsc.account",
	// 	// 			Amount: "0.001",
	// 	// 			Asset:  "hbd",
	// 	// 			NetId:  "vsc-mocknet",
	// 	// 			Nonce:  uint64(i),
	// 	// 		}
	// 	// 		sTx, _ := transactionCreator.SignFinal(transferOp)

	// 	// 		transactionCreator.Broadcast(sTx)
	// 	// 	}

	// 	// 	stakeOp := &transactionpool.VSCStake{
	// 	// 		From:   didKey.String(),
	// 	// 		To:     didKey.String(),
	// 	// 		Amount: "0.020",
	// 	// 		Asset:  "hbd",
	// 	// 		NetId:  "vsc-mocknet",
	// 	// 		Type:   "stake",
	// 	// 		Nonce:  uint64(5),
	// 	// 	}
	// 	// 	sTx, err := transactionCreator.SignFinal(stakeOp)

	// 	// 	fmt.Println("Sign error", err, sTx)

	// 	// 	stakeId, err := transactionCreator.Broadcast(sTx)

	// 	// 	fmt.Println("stakeId", stakeId, err)
	// 	// 	return nil
	// 	// },
	// 	r2e.Wait(40),
	// 	func() error {
	// 		stakeTx := &transactionpool.VscConsenusStake{
	// 			Account: "hive:test-account",
	// 			Amount:  "0.025",
	// 			Type:    "unstake",
	// 			NetId:   "vsc-mocknet",
	// 		}

	// 		ops, err := stakeTx.SerializeHive()

	// 		fmt.Println("consensus stake erro", err)
	// 		if err != nil {
	// 			panic(err)
	// 		}

	// 		tx := r2e.HiveCreator.MakeTransaction(ops)
	// 		r2e.HiveCreator.PopulateSigningProps(&tx, nil)
	// 		sig, _ := r2e.HiveCreator.Sign(tx)
	// 		tx.AddSig(sig)

	// 		unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
	// 		fmt.Println("stakeId", unstakeId)
	// 		return nil
	// 	},
	// 	// func() error {
	// 	// 	transferTx := &transactionpool.VSCTransfer{
	// 	// 		From:   didKey.String(),
	// 	// 		To:     "hive:vsc.account",
	// 	// 		Amount: "0.015",
	// 	// 		Asset:  "hbd_savings",
	// 	// 		NetId:  "vsc-mocknet",
	// 	// 		Nonce:  uint64(6),
	// 	// 	}
	// 	// 	sTx, _ := transactionCreator.SignFinal(transferTx)

	// 	// 	stakeId, err := transactionCreator.Broadcast(sTx)

	// 	// 	fmt.Println("transferStakeId", stakeId, err)
	// 	// 	return nil
	// 	// },
	// 	r2e.Wait(10),
	// 	func() error {
	// 		unstakeTx := &transactionpool.VSCStake{
	// 			From:   "hive:vsc.account",
	// 			To:     "hive:vsc.account",
	// 			Amount: "0.001",
	// 			Asset:  "hbd_savings",
	// 			Type:   "unstake",
	// 			NetId:  "vsc-mocknet",
	// 		}

	// 		ops, err := unstakeTx.SerializeHive()

	// 		if err != nil {

	// 			panic(err)
	// 		}

	// 		fmt.Println("Prefix: e2e-1] Unstake ops", ops, err)
	// 		tx := r2e.HiveCreator.MakeTransaction(ops)
	// 		r2e.HiveCreator.PopulateSigningProps(&tx, nil)
	// 		sig, _ := r2e.HiveCreator.Sign(tx)
	// 		tx.AddSig(sig)
	// 		unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
	// 		fmt.Println("unstakeId", unstakeId)

	// 		return nil
	// 	},
	// 	r2e.Wait(20),
	// 	func() error {
	// 		unstakeTx := &transactionpool.VSCStake{
	// 			From:   "hive:vsc.account",
	// 			To:     "hive:vsc.account",
	// 			Amount: "0.001",
	// 			Asset:  "hbd_savings",
	// 			Type:   "unstake",
	// 			NetId:  "vsc-mocknet",
	// 		}

	// 		ops, err := unstakeTx.SerializeHive()

	// 		if err != nil {

	// 			panic(err)
	// 		}

	// 		fmt.Println("Prefix: e2e-1] Unstake ops", ops, err)
	// 		tx := r2e.HiveCreator.MakeTransaction(ops)
	// 		r2e.HiveCreator.PopulateSigningProps(&tx, nil)
	// 		sig, _ := r2e.HiveCreator.Sign(tx)
	// 		tx.AddSig(sig)
	// 		unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
	// 		fmt.Println("unstakeId", unstakeId)

	// 		return nil
	// 	},
	// 	r2e.Wait(40),
	// 	func() error {
	// 		// mockCreator.Mr.MineNullBlocks(200)
	// 		return nil
	// 	},

	// 	func() error {
	// 		unstakeTx := &transactionpool.VSCStake{
	// 			From:   "hive:vsc.account",
	// 			To:     "hive:vsc.account",
	// 			Amount: "0.001",
	// 			Asset:  "hbd_savings",
	// 			Type:   "unstake",
	// 			NetId:  "vsc-mocknet",
	// 		}

	// 		ops, err := unstakeTx.SerializeHive()

	// 		if err != nil {

	// 			panic(err)
	// 		}

	// 		fmt.Println("Prefix: e2e-1] Unstake ops", ops, err)
	// 		tx := r2e.HiveCreator.MakeTransaction(ops)
	// 		r2e.HiveCreator.PopulateSigningProps(&tx, nil)
	// 		sig, _ := r2e.HiveCreator.Sign(tx)
	// 		tx.AddSig(sig)
	// 		unstakeId, _ := r2e.HiveCreator.Broadcast(tx)
	// 		fmt.Println("unstakeId", unstakeId)

	// 		return nil
	// 	},
	// 	r2e.Wait(20),
	// 	func() error {
	// 		mockCreator.ClaimInterest("vsc.gateway", 100)
	// 		return nil
	// 	},

	// 	r2e.Wait(20),
	// 	func() error {

	// 		fmt.Println("syncBalance depositing")

	// 		var demoAccounts = []string{
	// 			"vsc.staker1",
	// 			"vsc.staker2",
	// 			"vsc.staker3",
	// 			"vsc.staker4",
	// 			"vsc.staker5",
	// 			"vsc.staker6",
	// 		}

	// 		for _, account := range demoAccounts {
	// 			mockCreator.Transfer(account, "vsc.gateway", "100", "HBD", "")
	// 		}

	// 		return nil
	// 	},
	// 	r2e.Wait(90),
	// 	func() error {
	// 		mockCreator.ClaimInterest("vsc.gateway", 50)
	// 		return nil
	// 	},
	// 	// func() error {
	// 	// 	fmt.Println("Preparing atomicId")
	// 	// 	withdrawTx := &transactionpool.VscWithdraw{
	// 	// 		From:   "hive:test-account",
	// 	// 		To:     "hive:test-account",
	// 	// 		Amount: "0.030",
	// 	// 		Asset:  "hbd",
	// 	// 		NetId:  "vsc-mocknet",
	// 	// 		Type:   "withdraw",
	// 	// 	}
	// 	// 	withdrawTx2 := &transactionpool.VscWithdraw{
	// 	// 		From:   "hive:test-account",
	// 	// 		To:     "hive:test-account",
	// 	// 		Amount: "0.060",
	// 	// 		Asset:  "hbd",
	// 	// 		NetId:  "vsc-mocknet",
	// 	// 		Type:   "withdraw",
	// 	// 	}

	// 	// 	ops, _ := withdrawTx.SerializeHive()
	// 	// 	ops2, _ := withdrawTx2.SerializeHive()
	// 	// 	totalOps := []hivego.HiveOperation{}
	// 	// 	totalOps = append(totalOps, ops...)
	// 	// 	totalOps = append(totalOps, ops2...)

	// 	// 	tx := r2e.HiveCreator.MakeTransaction(totalOps)

	// 	// 	r2e.HiveCreator.PopulateSigningProps(&tx, nil)
	// 	// 	sig, _ := r2e.HiveCreator.Sign(tx)

	// 	// 	tx.AddSig(sig)

	// 	// 	atomicId, _ := r2e.HiveCreator.Broadcast(tx)
	// 	// 	fmt.Println("atomicId", atomicId)

	// 	// 	return nil
	// 	// },

	// 	// r2e.Produce(e2e.TimeToBlocks(time.Duration(time.Hour * 24 * 3))),
	// })

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
		NetId:  "vsc-mocknet",
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
