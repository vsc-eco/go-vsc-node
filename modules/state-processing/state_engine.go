package stateEngine

import (
	"encoding/json"
	"fmt"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/transactions"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
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
	contractHistory contracts.ContractHistory
	txDb            transactions.Transactions

	//Nonce map similar to what we use before
	NonceMap   map[string]int
	BalanceMap map[string]map[string]int64

	AnchoredHeight int
	VirtualRoot    []byte
	VirtualOutputs []byte
	VirtualState   []byte

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

			cj := CustomJson{
				Id:                   opVal["id"].(string),
				RequiredAuths:        arrayToStringArry(opVal["required_auths"].([]interface{})),
				RequiredPostingAuths: arrayToStringArry(opVal["required_posting_auths"].([]interface{})),
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

func New(da *DataLayer.DataLayer, witnessesDb witnesses.Witnesses, electionsDb elections.Elections, contractDb contracts.Contracts, txDb transactions.Transactions) StateEngine {
	return StateEngine{
		da: da,
		// db: db,

		witnessDb:  witnessesDb,
		electionDb: electionsDb,
		contractDb: contractDb,
		txDb:       txDb,
	}
}
