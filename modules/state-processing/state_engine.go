package stateEngine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/transactions"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const GATEWAY_WALLET = "vsc.gateway"

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
	contractHistory contracts.ContractHistory
	txDb            transactions.Transactions

	//Nonce map similar to what we use before
	NonceMap   map[string]int
	BalanceMap map[string]map[string]int64

	AnchoredHeight int
	VirtualRoot    []byte
	VirtualOutputs []byte
	VirtualState   []byte

	LedgerExecutor *LedgerExecutor

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
	// BalanceMap map[string]int -->
	// StateMerkle string <--
	// BalanceMapUpdates []interface <-- //Ledger ops
	// SideEffects []interface <-- //Placeholder for future
}

type AuthCheckType struct {
	Level    string
	Required []string
}

func AuthCheck(customJson CustomJson, args AuthCheckType) bool {
	// if args.Level != nil {

	// }
	//Write code for auth check
	return false
}

func arrayToStringArry(arr []interface{}) []string {
	out := make([]string, 0)
	for _, v := range arr {
		out = append(out, v.(string))
	}
	return out
}

func (se *StateEngine) ProcessTransfer() {

}

func (se *StateEngine) ProcessTx(tx hive_blocks.Tx, extraInfo ProcessExtraInfo) {
	//Detect Transaction Type

	if tx.Operations[0].Type == "custom_json" && len(tx.Operations) > 1 {
		headerOp := tx.Operations[0]
		Id := headerOp.Value["id"].(string)
		RequiredAuths := arrayToStringArry(headerOp.Value["required_auths"].(primitive.A))

		if (Id == "vsc.bridge_ref" || Id == "vsc.bridge_withdraw" || Id == "vsc.savings_withdraw") && RequiredAuths[0] == GATEWAY_WALLET {
			if tx.Operations[1].Type == "transfer" {
				//Process withdraw
			}
			if tx.Operations[1].Type == "transfer_from_savings" {
				//Process withdraw
			}
		}
	}

	for opIdx, op := range tx.Operations {
		if op.Type == "account_update" {
			// fmt.Println(op)
			opValue := op.Value

			if opValue["json_metadata"] != nil {
				// fmt.Println(opValue["posting_json_metadata"])
				untypedJson := make(map[string]interface{})

				bbytes := []byte(opValue["json_metadata"].(string))
				json.Unmarshal(bbytes, &untypedJson)

				if untypedJson["vsc_node"] != nil {
					rawJson := witnesses.PostingJsonMetadata{}
					json.Unmarshal(bbytes, &rawJson)

					inputData := witnesses.SetWitnessUpdateType{
						Account:  op.Value["account"].(string),
						Height:   extraInfo.BlockHeight,
						TxId:     tx.TransactionID,
						BlockId:  extraInfo.BlockId,
						Metadata: rawJson,
					}
					se.witnessDb.SetWitnessUpdate(inputData)
				}
			}
		}
		if op.Type == "custom_json" {
			opVal := op.Value

			fmt.Println(opVal["required_auths"])
			cj := CustomJson{
				Id:                   opVal["id"].(string),
				RequiredAuths:        arrayToStringArry(opVal["required_auths"].(primitive.A)),
				RequiredPostingAuths: arrayToStringArry(opVal["required_posting_auths"].(primitive.A)),
				Json:                 []byte(opVal["json"].(string)),
			}

			fmt.Println("cj.RequiredAuths", cj.RequiredAuths)
			txSelf := TxSelf{
				BlockHeight:   extraInfo.BlockHeight,
				BlockId:       extraInfo.BlockId,
				Timestamp:     extraInfo.Timestamp,
				Index:         tx.Index,
				OpIndex:       opIdx,
				TxId:          tx.TransactionID,
				RequiredAuths: cj.RequiredAuths,
			}

			var tx VSCTransaction
			//Definitely will be string
			if cj.Id == "vsc.tx" {
				tx = TxVscHive{
					Self: txSelf,
				}
				json.Unmarshal(cj.Json, &tx)
				fmt.Println(tx)

				//Hopefully contracts
				//Also transfer execution, and withdraw
			} else if cj.Id == "vsc.propose_block" {
				rawJson := map[string]interface{}{}
				json.Unmarshal(cj.Json, &rawJson)
				meinTx := TxProposeBlock{
					Self: txSelf,
					SignedBlock: SignedBlockHeader{
						UnsignedBlockHeader: UnsignedBlockHeader{
							Block: cid.MustParse(rawJson["signed_block"].(map[string]interface{})["block"].(string)),
						},
						Signature: dids.SerializedCircuit{
							BitVector: rawJson["signed_block"].(map[string]interface{})["signature"].(map[string]interface{})["bv"].(string),
							Signature: rawJson["signed_block"].(map[string]interface{})["signature"].(map[string]interface{})["sig"].(string),
						},
					},
				}
				json.Unmarshal(cj.Json, &meinTx)

				fmt.Println("vanechkin do you speak english?", meinTx)
				tx = meinTx
			} else if cj.Id == "vsc.election_result" {
				meinTx := TxElectionResult{
					Self: txSelf,
				}
				json.Unmarshal(cj.Json, &meinTx)
				fmt.Println(meinTx)
				tx = meinTx
			} else if cj.Id == "vsc.create_contract" {
				meinTx := TxCreateContract{
					Self: txSelf,
				}
				json.Unmarshal(cj.Json, &meinTx)

				tx = meinTx
			} else {
				//Unrecognized TX
				continue
			}

			fmt.Println("Executing se", tx)
			tx.ExecuteTx(se)
			// panic("Expected Panic!")
		}
		if op.Type == "transfer" || op.Type == "transfer_to_savings" {

			//TODO: Finish up support for directly handling staked transfers
			fmt.Println("Transfer Op", op)
			fmt.Println("Transfer Value")

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
					//TODO: Add failover logic to return the amount to the user in form of VSC balance
					return
				}
			}
			amount, _ := strconv.ParseInt(op.Value["amount"].(map[string]interface{})["amount"].(string), 10, 64)

			fmt.Println("Token", token, amount)

			if op.Value["to"] == "vsc.gateway" {
				se.LedgerExecutor.Deposit(Deposit{
					Id:     tx.TransactionID,
					Asset:  token,
					Amount: amount,
					From:   op.Value["from"].(string),
					Memo:   op.Value["memo"].(string),

					BIdx:  int64(tx.Index),
					OpIdx: int64(opIdx),
				})

				bbytes, _ := json.Marshal(se.LedgerExecutor.VirtualLedger)

				fmt.Println("bbytes string()", string(bbytes))
				time.Sleep(1 * time.Hour)
				panic("Hello")
			}
		}
	}
}

func (se *StateEngine) GetBalance(id string, asset string) int64 {

	return 0
}

func (se *StateEngine) Commit() {

}

func (se *StateEngine) ProcessBlock() {

}

func (se *StateEngine) Init() {

}

func (se *StateEngine) Start() *promise.Promise[any] {

	return nil
}

func (se *StateEngine) Stop() {

}

func New(da *DataLayer.DataLayer, witnessesDb witnesses.Witnesses, electionsDb elections.Elections, contractDb contracts.Contracts, txDb transactions.Transactions, ledgerDb ledgerDb.Ledger, balanceDb ledgerDb.Balances) StateEngine {
	return StateEngine{
		da: da,
		// db: db,

		witnessDb:  witnessesDb,
		electionDb: electionsDb,
		contractDb: contractDb,
		txDb:       txDb,

		LedgerExecutor: &LedgerExecutor{
			Ls: &LedgerSystem{
				BalanceDb: balanceDb,
				LedgerDb:  ledgerDb,
			},
		},
	}
}
