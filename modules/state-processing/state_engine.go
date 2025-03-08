package stateEngine

import (
	"crypto"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/chebyrash/promise"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type ProcessExtraInfo struct {
	BlockHeight int
	BlockId     string
	Timestamp   string
}

type StateResult struct {
}

type StateEngine struct {
	db              *vsc.VscDb
	da              *DataLayer.DataLayer
	witnessDb       witnesses.Witnesses
	electionDb      elections.Elections
	contractDb      contracts.Contracts
	contractState   contracts.ContractState
	txDb            transactions.Transactions
	hiveBlocks      hive_blocks.HiveBlocks
	vscBlocks       vscBlocks.VscBlocks
	interestClaimDb ledgerDb.InterestClaims

	//Nonce map similar to what we use before
	NonceMap map[string]int
	// BalanceMap map[string]map[string]int64

	AnchoredHeight uint64
	VirtualRoot    []byte
	VirtualOutputs []byte
	VirtualState   []byte
	VirtualOps     uint64

	LedgerExecutor *LedgerExecutor

	TxBatch []VSCTransaction

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
func (se *StateEngine) Finalize() {
	//Make it happen!
}

func (se *StateEngine) claimHBDInterest(blockHeight uint64, amount int64) {
	lastClaim := se.interestClaimDb.GetLastClaim(blockHeight)

	claimHeight := uint64(0)
	if lastClaim != nil {
		claimHeight = lastClaim.BlockHeight
	}

	se.LedgerExecutor.Ls.ClaimHBDInterest(claimHeight, blockHeight, amount)

	se.interestClaimDb.SaveClaim(blockHeight, amount)
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
// Nice right? No front-running!
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
			se.ExecuteBatch()
			se.slotStatus = &SlotStatus{
				SlotHeight: slotInfo.StartHeight,
				Done:       false,
			}
		}
	}

	for _, virtualOp := range block.VirtualOps {
		//Process virtual operations here such as claimed interest
		fmt.Println("registered vOP", virtualOp)

		if virtualOp.Op.Type == "interest_operation" {
			//Ensure it matches our gateway wallet
			if virtualOp.Op.Value["owner"].(string) == common.GATEWAY_WALLET {
				fmt.Println("virtualOp.Op.Value", virtualOp.Op.Value)

				amount, err := strconv.ParseInt(virtualOp.Op.Value["interest"].(map[string]any)["amount"].(string), 10, 64)

				if err != nil {
					//Should we panic here? ...No
					panic("Invalid Interest Amount. Possible deviation")
				}
				se.claimHBDInterest(blockInfo.BlockHeight, amount)
			}
		}
	}

	for blkIdx, tx := range block.Transactions {

		if tx.Operations[0].Type == "custom_json" && len(tx.Operations) > 1 {
			headerOp := tx.Operations[0]
			Id := headerOp.Value["id"].(string)
			RequiredAuths := arrayToStringArray(headerOp.Value["required_auths"].(primitive.A))

			if (Id == "vsc.bridge_ref" || Id == "vsc.bridge_withdraw" || Id == "vsc.savings_withdraw") && RequiredAuths[0] == common.GATEWAY_WALLET {
				if tx.Operations[1].Type == "transfer" {
					//Process withdraw
				}
				if tx.Operations[1].Type == "transfer_from_savings" {
					//Process withdraw
				}
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

		if singleOp.Type == "custom_json" {
			// fmt.Println(op.Type)
			opVal := singleOp.Value
			cj := CustomJson{
				Id:                   opVal["id"].(string),
				RequiredAuths:        arrayToStringArray(opVal["required_auths"]),
				RequiredPostingAuths: arrayToStringArray(opVal["required_posting_auths"]),
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
					se.slotStatus.Done = true
					se.slotStatus.Producer = cj.RequiredAuths[0]

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

					fmt.Println("Validated block?", validated)
					if validated {
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

				parsedTx.ExecuteTx(se)
				continue
			} else if cj.Id == "vsc.election_result" {
				parsedTx := &TxElectionResult{
					Self: txSelf,
				}
				json.Unmarshal(cj.Json, &parsedTx)
				parsedTx.ExecuteTx(se)
				continue
			}
			//# End parsing system transactions
		}

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
				amount, _ := strconv.ParseInt(op.Value["amount"].(map[string]interface{})["amount"].(string), 10, 64)
				// amount, _ := op.Value["amount"].(map[string]interface{})["amount"].(int64)

				if op.Value["to"] == "vsc.gateway" {
					leDeposit := Deposit{
						Id:     MakeTxId(tx.TransactionID, opIndex),
						Asset:  token,
						Amount: amount,
						From:   "hive:" + op.Value["from"].(string),
						Memo:   op.Value["memo"].(string),

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
					RequiredAuths:        arrayToStringArray(opVal["required_auths"]),
					RequiredPostingAuths: arrayToStringArray(opVal["required_posting_auths"]),
					Json:                 []byte(opVal["json"].(string)),
				}

				txSelf := TxSelf{
					TxId:          MakeTxId(tx.TransactionID, opIndex),
					BlockHeight:   blockInfo.BlockHeight,
					BlockId:       blockInfo.BlockId,
					Timestamp:     blockInfo.Timestamp,
					Index:         tx.Index,
					OpIndex:       opIndex,
					RequiredAuths: cj.RequiredAuths,
				}

				var vscTx VSCTransaction
				if cj.Id == "vsc.withdraw" {
					parsedTx := TxVSCWithdraw{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.call" {
					parsedTx := TxVscCallContract{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = parsedTx
				} else if cj.Id == "vsc.stake_hbd" {
					//Fill this in
				}

				se.TxBatch = append(se.TxBatch, vscTx)
			}
		}
	}

	//Executes user action when the slot has been completed
	if se.slotStatus.Done {
		se.ExecuteBatch()
	}
}

func (se *StateEngine) ExecuteBatch() {
	for _, tx := range se.TxBatch {
		// txSelf := tx.TxSelf()
		// fmt.Println("Batch executing", txSelf.BlockHeight, txSelf.TxId)
		tx.ExecuteTx(se)
	}

	se.TxBatch = make([]VSCTransaction, 0)
}

// Execute block within state engine as it is very important
func (se *StateEngine) ExecuteBlock(blockContent BlockContent) {

}

// If there is transactions in the queue, use the last vsc block height to resume
// If not continue parsing from lastBlk
// Need to test
func (se *StateEngine) SaveBlockHeight(lastBlk uint64, lastSavedBlk uint64) uint64 {
	if len(se.TxBatch) > 0 {
		vscRecord, _ := se.vscBlocks.GetBlockByHeight(lastBlk)
		if lastSavedBlk != uint64(vscRecord.EndBlock) {
			return uint64(vscRecord.EndBlock)
		} else {
			return lastSavedBlk
		}
	} else {
		return lastBlk
	}
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

func New(da *DataLayer.DataLayer,
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
) *StateEngine {
	return &StateEngine{
		da: da,
		// db: db,

		witnessDb:       witnessesDb,
		electionDb:      electionsDb,
		contractDb:      contractDb,
		contractState:   contractStateDb,
		hiveBlocks:      hiveBlocks,
		vscBlocks:       vscBlocks,
		interestClaimDb: interestClaims,
		txDb:            txDb,

		LedgerExecutor: &LedgerExecutor{
			Ls: &LedgerSystem{
				BalanceDb: balanceDb,
				LedgerDb:  ledgerDb,
			},
		},
	}
}
