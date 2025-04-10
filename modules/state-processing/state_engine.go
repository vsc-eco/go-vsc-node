package stateEngine

import (
	"crypto"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/logger"
	"vsc-node/modules/common"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	ledgerSystem "vsc-node/modules/ledger-system"
	rcSystem "vsc-node/modules/rc-system"
	wasm_parent_ipc "vsc-node/modules/wasm/parent_ipc"

	"github.com/chebyrash/promise"
)

type ProcessExtraInfo struct {
	BlockHeight int
	BlockId     string
	Timestamp   string
}

type StateResult struct {
}

type StateEngine struct {
	db            *vsc.VscDb
	da            *DataLayer.DataLayer
	witnessDb     witnesses.Witnesses
	electionDb    elections.Elections
	contractDb    contracts.Contracts
	contractState contracts.ContractState
	txDb          transactions.Transactions
	hiveBlocks    hive_blocks.HiveBlocks
	vscBlocks     vscBlocks.VscBlocks
	claimDb       ledgerDb.InterestClaims
	rcDb          rcDb.RcDb
	nonceDb       nonces.Nonces

	wasm *wasm_parent_ipc.Wasm

	//Nonce map similar to what we use before
	NonceMap map[string]int
	RcMap    map[string]int64
	rcSystem *rcSystem.RcSystem

	//Unused ideas
	// AnchoredHeight uint64
	// VirtualRoot    []byte
	// VirtualState   []byte
	// VirtualOps     uint64

	LedgerExecutor *LedgerExecutor

	//Atomic packet; aka -> array of transactions with ops in each
	TxBatch  []TxPacket
	TxOutIds []string

	//Map of txId --> output
	TxOutput        map[string]TxOutput
	ContractOutputs map[string]*contract_session.TempOutput
	ContractResults map[string][]TxResultWithId

	//First Tx of batch
	firstTxHeight uint64

	log logger.Logger

	slotStatus *SlotStatus

	BlockHeight int
}

//Transaction
// InputArgs string -->
// // - Entrypoint
// // - Args
// // - Contract ID
// // - Account auths
// Intents []interface -->

// //Pass yes or no
// Output interface <--
// // - error
// // - logs
// // - ok: bool

// //State changes
// StateMerkle string <--
// BalanceMapUpdates []interface <-- //Ledger ops
// SideEffects []interface <-- //Placeholder for future

// Finalizes state into pseudo block

func (se *StateEngine) claimHBDInterest(blockHeight uint64, amount int64) {
	lastClaim := se.claimDb.GetLastClaim(blockHeight - 1)

	claimHeight := uint64(0)
	if lastClaim != nil {
		claimHeight = lastClaim.BlockHeight
	}

	se.LedgerExecutor.Ls.ClaimHBDInterest(claimHeight, blockHeight, amount)
}

// Gets ranomized schedule of witnesses
// Uses a different PRNG variant from the original used in JS VSC
// Not aiming for exact replica
func (se *StateEngine) GetSchedule(slotHeight uint64) []WitnessSlot {
	lastElection, err := se.electionDb.GetElectionByHeight(slotHeight)

	if err != nil {
		return nil
	}

	witnessList := make([]Witness, 0)

	for _, v := range lastElection.Members {
		witnessList = append(witnessList, Witness{
			Key:     v.Key,
			Account: v.Account,
		})
	}

	//Use the slot height to calculate round start and finish.
	//Slot height is consistent.
	roundInfo := vscBlocks.CalculateRoundInfo(slotHeight)

	randBlock := roundInfo.StartHeight - 1

	hiveBlock, err := se.hiveBlocks.GetBlock(randBlock)

	hash := crypto.SHA256.New()
	if err != nil {
		//Defualt seed
		//This will only happen if the network is very new within the first 1 hour of creation
		hash.Write([]byte("VSC.NETWORK"))
	} else {
		hash.Write([]byte(hiveBlock.BlockID))
	}
	hash.Write([]byte(strconv.Itoa(int(roundInfo.StartHeight))))

	seed := hash.Sum(nil)

	var seed32 [32]byte
	// var seed2 [32]byte
	copy(seed32[:], seed)
	witnessSchedule := GenerateSchedule(slotHeight, witnessList, seed32)

	return witnessSchedule
}

// Implementation note:
// You might be wondering why there is a side batch of TXs which awaits new block processing
// This is done to prevent needing to scan the transaction pool upon node restart
// Instead, this scans the blocks that have been indexed to get the correct state
// However, if we did the original approach of constant execution of hive transactions...
// ...even if no VSC block, that would result in needing scan blocks before the last VSC block to get the correct state
// Even in that scenario, we would still need to scan backwards to get the "wet" state of the stateEngine
// In other words, it adds complexity to the state engine while being less efficient.
// This model is more efficient and best yet, it prevents MEV potential by locking the block execution time to the witness slot.
func (se *StateEngine) ProcessBlock(block hive_blocks.HiveBlock) {
	blockInfo := struct {
		BlockHeight uint64
		BlockId     string
		Timestamp   string
	}{
		BlockHeight: block.BlockNumber,
		BlockId:     block.BlockID,
		Timestamp:   block.Timestamp,
	}

	//What is active slot?
	// bh = 5
	// 0 - 10
	// prev blk = 1,
	slotInfo := CalculateSlotInfo(block.BlockNumber)
	if se.slotStatus == nil {
		se.slotStatus = &SlotStatus{
			SlotHeight: slotInfo.StartHeight,
			Done:       false,
		}
	} else {
		if se.slotStatus.SlotHeight != slotInfo.StartHeight {
			//Updates balances index before next batch can execute
			vscBlock, _ := se.vscBlocks.GetBlockByHeight(se.slotStatus.SlotHeight - 1)

			startBlock := uint64(0)
			if vscBlock != nil {
				startBlock = uint64(vscBlock.EndBlock)
			}

			se.UpdateBalances(startBlock, se.slotStatus.SlotHeight)

			se.UpdateRcMap(se.slotStatus.SlotHeight)

			se.RcMap = make(map[string]int64)

			se.ExecuteBatch()

			se.slotStatus = &SlotStatus{
				SlotHeight: slotInfo.StartHeight,
				Done:       false,
			}
		}
	}

	for _, virtualOp := range block.VirtualOps {
		//Process virtual operations here such as claimed interest

		fmt.Println("block.VirtualOps", block.VirtualOps)
		if virtualOp.Op.Type == "interest_operation" {
			//Ensure it matches our gateway wallet
			if virtualOp.Op.Value["owner"].(string) == common.GATEWAY_WALLET {

				vInt := virtualOp.Op.Value["interest"].(map[string]any)["interest"].(int)

				// if err != nil {
				// 	//Should we panic here? ...No
				// 	panic("Invalid Interest Amount. Possible deviation")
				// }
				se.claimHBDInterest(blockInfo.BlockHeight, int64(vInt))
			}
		}
	}

	for blkIdx, tx := range block.Transactions {

		if tx.Operations[0].Type == "custom_json" {
			headerOp := tx.Operations[0]
			Id := headerOp.Value["id"].(string)
			opVal := headerOp.Value
			RequiredAuths := common.ArrayToStringArray(opVal["required_auths"])

			cj := CustomJson{
				Id:                   opVal["id"].(string),
				RequiredAuths:        common.ArrayToStringArray(opVal["required_auths"]),
				RequiredPostingAuths: common.ArrayToStringArray(opVal["required_posting_auths"]),
				Json:                 []byte(opVal["json"].(string)),
			}

			if Id == "vsc.fr_sync" && RequiredAuths[0] == common.GATEWAY_WALLET {
				se.log.Debug("vsc.fr_sync", opVal)

				frSync := struct {
					StakedAmount   int64 `json:"stake_amt"`
					UnstakedAmount int64 `json:"unstake_amt"`
				}{}
				json.Unmarshal(cj.Json, &frSync)

				fmt.Println("frSync", frSync)

				var amt int64

				if frSync.StakedAmount > 0 {
					amt = frSync.StakedAmount
				} else {
					//Must be negative to indicate unstaking has occurred
					amt = -frSync.UnstakedAmount
				}

				se.LedgerExecutor.Ls.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
					Id:          MakeTxId(tx.TransactionID, 0),
					Amount:      amt,
					BlockHeight: blockInfo.BlockHeight + 1,
					Owner:       common.FR_VIRTUAL_ACCOUNT,
					//Fractional reserve update
					Asset: "hbd_savings",
					Type:  "fr_sync",
				})
			}

			if (Id == "vsc.actions") && RequiredAuths[0] == common.GATEWAY_WALLET {

				// txSelf := TxSelf{
				// 	BlockHeight:   blockInfo.BlockHeight,
				// 	BlockId:       blockInfo.BlockId,
				// 	Timestamp:     blockInfo.Timestamp,
				// 	Index:         blkIdx,
				// 	OpIndex:       0,
				// 	TxId:          tx.TransactionID,
				// 	RequiredAuths: cj.RequiredAuths,
				// }

				actionUpdate := map[string]interface{}{}
				json.Unmarshal(cj.Json, &actionUpdate)

				se.LedgerExecutor.Ls.IndexActions(actionUpdate, ExtraInfo{
					block.BlockNumber,
				})

				// if tx.Operations[1].Type == "transfer" {
				// 	//Process withdraw
				// }
				// if tx.Operations[1].Type == "transfer_from_savings" {
				// 	//Process withdraw
				// }
				// if tx.Operations[1].Type == "transfer_to_savings" {
				// 	//Process deposit
				// }
			}
		}

		//Start by looking for block production
		singleOp := tx.Operations[0]

		//Main pipeline
		if singleOp.Type == "account_update" {
			opValue := singleOp.Value

			if opValue["json_metadata"] != nil {
				untypedJson := make(map[string]interface{})

				bbytes := []byte(opValue["json_metadata"].(string))
				json.Unmarshal(bbytes, &untypedJson)

				rawJson := witnesses.PostingJsonMetadata{}
				json.Unmarshal(bbytes, &rawJson)

				if slices.Contains(rawJson.Services, "vsc.network") {
					inputData := witnesses.SetWitnessUpdateType{
						Account:  singleOp.Value["account"].(string),
						Height:   blockInfo.BlockHeight,
						TxId:     tx.TransactionID,
						BlockId:  blockInfo.BlockId,
						Metadata: rawJson,
					}
					se.witnessDb.SetWitnessUpdate(inputData)
				}
			}
			continue
		}

		var lastblock uint64
		vscBlock, _ := se.vscBlocks.GetBlockByHeight(blockInfo.BlockHeight)
		if vscBlock != nil {
			lastblock = uint64(vscBlock.EndBlock)
		}

		session := se.LedgerExecutor.NewSession(lastblock)
		if singleOp.Type == "custom_json" {
			// fmt.Println(op.Type)
			opVal := singleOp.Value
			cj := CustomJson{
				Id:                   opVal["id"].(string),
				RequiredAuths:        common.ArrayToStringArray(opVal["required_auths"]),
				RequiredPostingAuths: common.ArrayToStringArray(opVal["required_posting_auths"]),
				Json:                 []byte(opVal["json"].(string)),
			}

			txSelf := TxSelf{
				BlockHeight:   blockInfo.BlockHeight,
				BlockId:       blockInfo.BlockId,
				Timestamp:     blockInfo.Timestamp,
				Index:         blkIdx,
				OpIndex:       0,
				TxId:          tx.TransactionID,
				RequiredAuths: cj.RequiredAuths,
			}
			//Start parsing block
			if cj.Id == "vsc.produce_block" {
				//Process block production
				rawJson := map[string]interface{}{}
				json.Unmarshal(cj.Json, &rawJson)
				// parsedTx := TxProposeBlock{}
				// json.Unmarshal(cj.Json, &parsedTx)

				// parsedTx.ExecuteTx(se)

				schedule := se.GetSchedule(slotInfo.StartHeight)

				var scheduleSlot WitnessSlot

				for _, slot := range schedule {
					if slot.SlotHeight == slotInfo.StartHeight {
						scheduleSlot = slot
						break
					}
				}

				// fmt.Println("cj.RequiredAuths[0], scheduleSlot.Account ", cj.RequiredAuths[0], scheduleSlot.Account)
				if cj.RequiredAuths[0] == scheduleSlot.Account {
					rawJson := map[string]interface{}{}
					json.Unmarshal(cj.Json, &rawJson)

					parsedBlock := TxProposeBlock{
						Self: txSelf,
						SignedBlock: SignedBlockHeader{
							UnsignedBlockHeader: UnsignedBlockHeader{},
							Signature:           dids.SerializedCircuit{},
						},
					}
					json.Unmarshal(cj.Json, &parsedBlock)

					validated := parsedBlock.Validate(se)

					// fmt.Println("Validated block?", validated)
					if validated {
						se.slotStatus.Done = true
						se.slotStatus.Producer = cj.RequiredAuths[0]
						parsedBlock.ExecuteTx(se)
					}
				}
				continue
			}
			//# End parsing block

			//# Start parsing system transactions
			if cj.Id == "vsc.create_contract" {
				parsedTx := TxCreateContract{
					Self: txSelf,
				}
				json.Unmarshal(cj.Json, &parsedTx)

				parsedTx.ExecuteTx(se, session, nil, nil)
				continue
			} else if cj.Id == "vsc.election_result" {
				parsedTx := &TxElectionResult{
					Self: txSelf,
				}
				json.Unmarshal(cj.Json, &parsedTx)
				parsedTx.ExecuteTx(se, session, nil)
				continue
			}
			//# End parsing system transactions
		}

		opList := make([]VSCTransaction, 0)

		for opIndex, op := range tx.Operations {

			//# Start parsing gateway transfer operations
			if op.Type == "transfer" {

				if op.Value["from"] == "vsc.gateway" {
					continue
				}

				//TODO: Finish up support for directly handling staked transfers

				var token string
				if op.Value["amount"].(map[string]interface{})["nai"] == "@@000000021" {
					token = "hive"

				} else if op.Value["amount"].(map[string]interface{})["nai"] == "@@000000013" {
					token = "hbd"
				}

				if op.Type == "transfer_to_savings" && token == "hbd" {
					if token == "hbd" {
						//Labeled as savings as there can be hbd savings, hive savings, and hive staked, but not hbd staked (within hive)
						//Only HBD savings generates APR
						token = "hbd_savings"
					} else {
						//Potentially add failover logic
						//However, balance is considered "untracked" if it is Hive token deposited directly to savings

						continue
					}
				}
				fmt.Println("Deposit info", op.Value)
				amount, _ := strconv.ParseFloat(op.Value["amount"].(map[string]interface{})["amount"].(string), 64)

				amt := int64(amount)

				if op.Value["to"] == "vsc.gateway" {
					leDeposit := Deposit{
						Id:     MakeTxId(tx.TransactionID, opIndex),
						Asset:  token,
						Amount: amt,
						From:   "hive:" + op.Value["from"].(string),
						Memo:   op.Value["memo"].(string),

						BlockHeight: blockInfo.BlockHeight,

						BIdx:  int64(tx.Index),
						OpIdx: int64(opIndex),
					}
					fmt.Println("Registering deposit!", leDeposit)
					se.LedgerExecutor.Deposit(leDeposit)
				}
			}
			//# End parsing gateway transfer operations

			//# Start parsing onchain user operations
			if op.Type == "custom_json" {
				opVal := op.Value
				cj := CustomJson{
					Id:                   opVal["id"].(string),
					RequiredAuths:        common.ArrayToStringArray(opVal["required_auths"]),
					RequiredPostingAuths: common.ArrayToStringArray(opVal["required_posting_auths"]),
					Json:                 []byte(opVal["json"].(string)),
				}

				for idx, auth := range cj.RequiredAuths {
					cj.RequiredAuths[idx] = "hive:" + auth
				}

				for idx, auth := range cj.RequiredPostingAuths {
					cj.RequiredPostingAuths[idx] = "hive:" + auth
				}

				txSelf := TxSelf{
					TxId:                 MakeTxId(tx.TransactionID, opIndex),
					BlockHeight:          blockInfo.BlockHeight,
					BlockId:              blockInfo.BlockId,
					Timestamp:            blockInfo.Timestamp,
					Index:                tx.Index,
					OpIndex:              opIndex,
					RequiredAuths:        cj.RequiredAuths,
					RequiredPostingAuths: cj.RequiredPostingAuths,
				}

				var vscTx VSCTransaction
				if cj.Id == "vsc.withdraw" {
					parsedTx := TxVSCWithdraw{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					// se.log.Debug("parsedTx vsc.withdraw", parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.call" {
					parsedTx := TxVscCallContract{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = parsedTx
				} else if cj.Id == "vsc.transfer" {
					parsedTx := TxVSCTransfer{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.stake_hbd" {
					//Fill this in
					parsedTx := TxStakeHbd{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.unstake_hbd" {
					// se.log.Debug("vsc.unstake_hbd", cj.Json)
					parsedTx := TxUnstakeHbd{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.consensus_stake" {
					parsedTx := TxConsensusStake{
						Self: txSelf,
					}

					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.consensus_unstake" {
					parsedTx := TxConsensusUnstake{
						Self: txSelf,
					}

					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				}

				if vscTx != nil {
					opList = append(opList, vscTx)
				}
			}
		}

		//Do not push empty tx packets
		if len(opList) > 0 {
			se.TxBatch = append(se.TxBatch, TxPacket{
				TxId: tx.TransactionID,
				Ops:  opList,
			})
			for idx, vscTx := range opList {
				txData := vscTx.ToData()
				txData["type"] = vscTx.Type()
				fmt.Println("Ingesting Hive tx")

				opIdx := int64(vscTx.TxSelf().OpIndex)
				blkIdx := int64(vscTx.TxSelf().Index)
				se.txDb.Ingest(transactions.IngestTransactionUpdate{
					Id:             MakeTxId(vscTx.TxSelf().TxId, idx),
					RequiredAuths:  vscTx.TxSelf().RequiredAuths,
					Status:         "INCLUDED",
					Type:           "hive",
					Tx:             txData,
					AnchoredBlock:  &block.BlockID,
					AnchoredHeight: &block.BlockNumber,
					AnchoredOpIdx:  &opIdx,
					AnchoredIndex:  &blkIdx,
				})
			}
		}
	}

	//Executes user action when the slot has been completed
	if se.slotStatus.Done {
		// vscBlock, _ := se.vscBlocks.GetBlockByHeight(se.slotStatus.SlotHeight - 1)

		// startBlock := uint64(0)
		// if vscBlock != nil {
		// 	startBlock = uint64(vscBlock.EndBlock)
		// }

		// se.UpdateBalances(startBlock, se.slotStatus.SlotHeight)

		se.ExecuteBatch()
		//Balances must be updated after the current slot has been fully executed
	}
}

func (se *StateEngine) ExecuteBatch() {

	lastBlock, _ := se.vscBlocks.GetBlockByHeight(se.slotStatus.SlotHeight)

	var lastBlockBh uint64
	if lastBlock == nil {
		lastBlockBh = 0
	} else {
		lastBlockBh = uint64(lastBlock.EndBlock)
	}

	types := make([]string, 0)
	for _, tx := range se.TxBatch {
		for _, vscTx := range tx.Ops {
			types = append(types, vscTx.Type())
		}
	}

	// if len(se.TxOutput) > 0 {
	// 	se.log.Debug("TxOutput pending", se.TxOutput, len(se.TxBatch))
	// }

	for idx, tx := range se.TxBatch {
		fmt.Println("Executing item in batch", idx, len(se.TxBatch))
		ledgerSession := se.LedgerExecutor.NewSession(lastBlockBh)
		rcSession := se.rcSystem.NewSession(ledgerSession)

		contractSessions := make(map[string]*contract_session.ContractSession)

		for k, v := range se.ContractOutputs {
			val := *v

			sess := contract_session.New(se.da)
			sess.FromOutput(val)
			contractSessions[k] = sess
		}

		logs := make([]string, 0)
		ok := true
		for idx, vscTx := range tx.Ops {
			if se.firstTxHeight == 0 {
				se.firstTxHeight = vscTx.TxSelf().BlockHeight - 1
			}

			var contractSession *contract_session.ContractSession
			var contractId string
			if vscTx.Type() == "call_contract" {
				contractCall, ok := vscTx.(TxVscCallContract)
				if ok {
					contractId = contractCall.ContractId
					if contractSessions[contractCall.ContractId] == nil {
						contractOutput := se.contractState.GetLastOutput(contractCall.ContractId, lastBlockBh)

						tmpOut := contract_session.TempOutput{
							Cache:     make(map[string][]byte),
							Metadata:  make(map[string]interface{}),
							Deletions: make(map[string]bool),
							Cid:       contractOutput.StateMerkle,
						}

						sess := contract_session.New(se.da)

						sess.FromOutput(tmpOut)
						contractSessions[contractCall.ContractId] = sess
					}
					contractSession = contractSessions[contractCall.ContractId]
				}
			}

			result := vscTx.ExecuteTx(se, ledgerSession, rcSession, contractSession)
			logs = append(logs, result.Ret)
			payer := vscTx.TxSelf().RequiredAuths[0]

			se.log.Debug("TRANSACTION STATUS", result, ledgerSession, "idx=", idx, vscTx.Type())
			fmt.Println("RC Payer is", payer, vscTx.Type(), vscTx, result.RcUsed)

			se.RcMap[payer] = se.RcMap[payer] + result.RcUsed

			if vscTx.Type() == "call_contract" {
				se.AppendOutput(contractId, TxResultWithId{
					TxId:    MakeTxId(tx.TxId, idx),
					Ret:     result.Ret,
					Success: result.Success,
				})
			}
			if !result.Success {
				se.log.Debug("TRANSACTION REVERTING")
				ok = false
				ledgerSession.Revert()
				break
			}
		}
		if ok {
			for k, v := range contractSessions {
				tmpOut := v.ToOutput()
				se.ContractOutputs[k] = &tmpOut
			}
		}
		ledgerIds := ledgerSession.Done()

		se.TxOutput[tx.TxId] = TxOutput{
			Ok:        ok,
			Logs:      logs,
			LedgerIds: ledgerIds,
		}
		se.TxOutIds = append(se.TxOutIds, tx.TxId)
	}

	se.TxBatch = make([]TxPacket, 0)
}

func (se *StateEngine) UpdateBalances(startBlock, endBlock uint64) {
	//Sets a default start block of 0 if near block 0
	//E2E testing starts at block 0
	var stBlock uint64
	if startBlock == 0 {
		stBlock = 0
	} else {
		stBlock = endBlock - 9
	}

	election, _ := se.electionDb.GetElectionByHeight(endBlock)
	records, _ := se.LedgerExecutor.Ls.ActionsDb.GetPendingActionsByEpoch(election.Epoch, "consensus_unstake")

	completeIds := make([]string, 0)
	ledgerRecords := make([]ledgerDb.LedgerRecord, 0)
	for _, record := range records {
		completeIds = append(completeIds, record.Id)

		ledgerRecords = append(ledgerRecords, ledgerDb.LedgerRecord{
			Id:          record.Id + "#out",
			Amount:      record.Amount,
			Asset:       "hive",
			BlockHeight: endBlock,
			Owner:       record.To,
		})
	}
	se.LedgerExecutor.Ls.LedgerDb.StoreLedger(ledgerRecords...)
	se.LedgerExecutor.Ls.ActionsDb.ExecuteComplete(completeIds...)

	//se.log.Debug("stBlock, endBlock", stBlock, endBlock)
	distinctAccounts, _ := se.LedgerExecutor.Ls.LedgerDb.GetDistinctAccountsRange(stBlock, endBlock)

	assets := []string{"hbd", "hive", "hbd_savings", "hive_consensus"}

	//Cleanup!
	for _, k := range distinctAccounts {
		ledgerBalances := map[string]int64{}
		prevBalRecord, _ := se.LedgerExecutor.Ls.BalanceDb.GetBalanceRecord(k, endBlock)
		var balanceR ledgerDb.BalanceRecord
		var stHeight uint64
		if prevBalRecord != nil {
			balanceR = *prevBalRecord
			stHeight = prevBalRecord.BlockHeight + 1 //Must add one to prevent querying ledger results from same bal record
		}
		for _, asset := range assets {
			if asset == "hbd" {
				ledgerBalances[asset] = balanceR.HBD
			} else if asset == "hive" {
				ledgerBalances[asset] = balanceR.Hive
			} else if asset == "hbd_savings" {
				ledgerBalances[asset] = balanceR.HBD_SAVINGS
			} else if asset == "hive_consensus" {
				ledgerBalances[asset] = balanceR.HIVE_CONSENSUS
			} else {
				ledgerBalances[asset] = 0
			}
		}
		//As of block X or below
		// se.LedgerExecutor.Ls.log.Debug("GetBalance for account", stBlock, stHeight, endBlock)

		ledgerUpdates, _ := se.LedgerExecutor.Ls.LedgerDb.GetLedgerRange(k, stHeight, endBlock, "")

		if len(*ledgerUpdates) == 0 {
			continue
		}

		for _, v := range *ledgerUpdates {
			ledgerBalances[v.Asset] += v.Amount
		}

		//Previous claim record
		claimRecord := se.claimDb.GetLastClaim(endBlock)
		// var lastClaim = int64(1)

		var hbdAvg = int64(0)
		var modifyHeight = uint64(0)
		var claimHeight = uint64(0)

		exists := claimRecord != nil
		var claimRecordC ledgerDb.ClaimRecord
		if exists {
			claimRecordC = *claimRecord
			modifyHeight = endBlock
		}

		if claimRecordC.BlockHeight != balanceR.HBD_CLAIM_HEIGHT {
			//Need to execute recalculation of the claim
			hbdAvg = 0
			claimHeight = claimRecord.BlockHeight
		} else if prevBalRecord != nil {
			//There is a previous balance record

			if ledgerBalances["hbd_savings"] != prevBalRecord.HBD_SAVINGS {
				//If there is a change in the savings balance, calculate the average

				A := endBlock - prevBalRecord.HBD_MODIFY_HEIGHT //Example: Modifed at 10, endBlock = 40; thus 10-40 = 30
				B := endBlock - prevBalRecord.HBD_CLAIM_HEIGHT  //Example: Claimed at block 100, current block 800; thus

				moreAvg := prevBalRecord.HBD_SAVINGS * int64(A) / int64(B)

				var tmpAvg int64
				if moreAvg > 0 {
					tmpAvg = prevBalRecord.HBD_AVG + moreAvg
				} else {
					tmpAvg = prevBalRecord.HBD_AVG
				}

				//Apply adjustments
				tmpAvg = tmpAvg * int64(A) / int64(B)
				if tmpAvg > 0 || prevBalRecord.HBD_MODIFY_HEIGHT == 0 {
					modifyHeight = endBlock
					hbdAvg = tmpAvg
				} else {
					modifyHeight = prevBalRecord.HBD_MODIFY_HEIGHT
					// hbdAvg = prevBalRecord.HBD_AVG
				}

				//Only update modified height if there is an average of >1 or 0.001
				//This is avoid the scenario of small deposits within a short period of time causing averages to be lost

				//Balance adjusted once accounting for the modify time
				//This ensures an average decreases. If balance is equal to the previous balance, then the average will not change
				// se.log.Debug("k="+k, "adjustedBal", tmpAvg, "hbdAvg="+strconv.Itoa(int(hbdAvg)), "B="+strconv.Itoa(int(B)), "C="+strconv.Itoa(int(C)), "endBlock="+strconv.Itoa(int(endBlock)))
				// se.log.Debug(prevBalRecord.HBD_CLAIM_HEIGHT, prevBalRecord.HBD_MODIFY_HEIGHT, moreAvg)
			} else {
				modifyHeight = prevBalRecord.HBD_MODIFY_HEIGHT
				hbdAvg = prevBalRecord.HBD_AVG
			}
		} else {
			modifyHeight = endBlock
			hbdAvg = 0
		}

		newRecord := ledgerDb.BalanceRecord{
			Account:        k,
			BlockHeight:    endBlock,
			Hive:           ledgerBalances["hive"],
			HIVE_CONSENSUS: ledgerBalances["hive_consensus"],
			HBD:            ledgerBalances["hbd"],
			HBD_SAVINGS:    ledgerBalances["hbd_savings"],
			HBD_AVG:        hbdAvg,
		}

		newRecord.HBD_MODIFY_HEIGHT = modifyHeight

		newRecord.HBD_CLAIM_HEIGHT = claimHeight

		se.LedgerExecutor.Ls.BalanceDb.UpdateBalanceRecord(newRecord)

		se.LedgerExecutor.VirtualLedger[k] = slices.DeleteFunc(se.LedgerExecutor.VirtualLedger[k], func(v ledgerSystem.LedgerUpdate) bool {
			return v.Type == "deposit"
		})
	}
}

func (se *StateEngine) UpdateRcMap(blockHeight uint64) {
	for k, v := range se.RcMap {
		//Get the last rc record
		rcRecord, _ := se.rcDb.GetRecord(k, blockHeight-1)

		var rcBal int64
		if rcRecord.BlockHeight == 0 {
			rcBal = v
		} else {
			frozeAmt := rcSystem.CalculateFrozenBal(rcRecord.BlockHeight, blockHeight, rcRecord.Amount)

			rcBal = frozeAmt + v
		}
		se.rcDb.SetRecord(k, blockHeight, rcBal)
	}
}

// Appends an output to the output map
func (se *StateEngine) AppendOutput(contractId string, out TxResultWithId) {
	if se.ContractResults[contractId] == nil {
		se.ContractResults[contractId] = make([]TxResultWithId, 0)
	}
	se.ContractResults[contractId] = append(se.ContractResults[contractId], out)
}

func (se *StateEngine) Flush() {
	se.ContractResults = make(map[string][]TxResultWithId)
	se.ContractOutputs = make(map[string]*contract_session.TempOutput)
	se.TxOutput = make(map[string]TxOutput)
	se.TxOutIds = make([]string, 0)
	se.firstTxHeight = 0
}

// If there is transactions in the queue, use the last vsc block height to resume
// If not continue parsing from lastBlk
// Need to test
func (se *StateEngine) SaveBlockHeight(lastBlk uint64, lastSavedBlk uint64) uint64 {

	if lastBlk == 0 || lastSavedBlk == 0 {
		fmt.Println("Returning lastSavdBlk", lastBlk, lastSavedBlk)
		return lastSavedBlk
	}
	var outputExists bool
	for _, _ = range se.TxOutput {
		outputExists = true
		break
	}
	if outputExists {
		vscRecord, _ := se.vscBlocks.GetBlockByHeight(lastBlk)
		if vscRecord != nil {
			if lastSavedBlk != uint64(vscRecord.SlotHeight) {
				return uint64(vscRecord.SlotHeight) + 1
			} else {
				return lastSavedBlk
			}
		} else {
			return se.firstTxHeight - 1
		}
	} else {
		return lastBlk
	}

	// if len(se.TxBatch) > 0 {
	// } else {
	// 	return lastBlk
	// }
}

func (se *StateEngine) Commit() {

}

func (se *StateEngine) Init() error {
	return nil
}

func (se *StateEngine) Start() *promise.Promise[any] {

	return nil
}

func (se *StateEngine) Stop() error {
	return nil
}

func New(logger logger.Logger, da *DataLayer.DataLayer,
	witnessesDb witnesses.Witnesses,
	electionsDb elections.Elections,
	contractDb contracts.Contracts,
	contractStateDb contracts.ContractState,
	txDb transactions.Transactions,
	ledgerDb ledgerDb.Ledger,
	balanceDb ledgerDb.Balances,
	hiveBlocks hive_blocks.HiveBlocks,
	interestClaims ledgerDb.InterestClaims,
	vscBlocks vscBlocks.VscBlocks,
	actionDb ledgerDb.BridgeActions,
	rcDb rcDb.RcDb,
	nonceDb nonces.Nonces,
	wasm *wasm_parent_ipc.Wasm,
) *StateEngine {
	return &StateEngine{
		log:             logger,
		TxOutput:        make(map[string]TxOutput),
		ContractResults: make(map[string][]TxResultWithId),
		ContractOutputs: make(map[string]*contract_session.TempOutput),
		TxOutIds:        make([]string, 0),

		da: da,
		// db: db,

		witnessDb:     witnessesDb,
		electionDb:    electionsDb,
		contractDb:    contractDb,
		contractState: contractStateDb,
		hiveBlocks:    hiveBlocks,
		vscBlocks:     vscBlocks,
		claimDb:       interestClaims,
		txDb:          txDb,
		rcDb:          rcDb,
		nonceDb:       nonceDb,
		rcSystem:      rcSystem.New(rcDb),

		wasm: wasm,

		LedgerExecutor: &LedgerExecutor{
			VirtualLedger: make(map[string][]ledgerSystem.LedgerUpdate),
			Ls: &LedgerSystem{
				BalanceDb: balanceDb,
				LedgerDb:  ledgerDb,
				ClaimDb:   interestClaims,
				ActionsDb: actionDb,
				log:       logger,
			},
		},
	}
}
