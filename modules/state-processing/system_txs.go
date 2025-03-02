package stateEngine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/transactions"

	"github.com/btcsuite/btcutil/bech32"
	"github.com/ipfs/go-cid"
	dagCbor "github.com/ipfs/go-ipld-cbor"
	mh "github.com/multiformats/go-multihash"
)

type ContractOutput struct {
	Id         string
	ContractId string   `json:"contract_id"`
	Inputs     []string `json:"inputs"`
	IoGas      int64    `json:"io_gas"`
	//This might not be used
	RemoteCalls []string         `json:"remote_calls"`
	Results     []ContractResult `json:"results"`
	StateMerkle string           `json:"state_merkle"`
}

func (output *ContractOutput) Ingest(se *StateEngine, txSelf TxSelf) {
	for idx, InputId := range output.Inputs {
		se.txDb.SetOutput(transactions.SetOutputUpdate{
			Id:       InputId,
			OutputId: output.Id,
			Index:    int64(idx),
		})
	}
	//Set output history
	se.contractState.IngestOutput(contracts.IngestOutputArgs{
		Id:         output.Id,
		ContractId: output.ContractId,
		Inputs:     output.Inputs,
		Gas: struct {
			IO int64
		}{
			IO: output.IoGas,
		},
		AnchoredBlock:  txSelf.BlockId,
		AnchoredHeight: int64(txSelf.BlockHeight),
		AnchoredId:     txSelf.TxId,
		AnchoredIndex:  int64(txSelf.Index),
	})
}

type ContractResult struct {
	IOGas     int           `type:"IOGas"`
	Error     string        `json:"error"`
	ErrorType string        `json:"errorType"`
	Ledger    []interface{} `json:"ledger"`
	Logs      string        `json:"logs"`
	Ret       string        `json:"ret"`
}

type TxCreateContract struct {
	Self TxSelf

	Version      string       `json:"__v"`
	NetId        string       `json:"net_id"`
	Name         string       `json:"name"`
	Code         string       `json:"code"`
	Owner        string       `json:"owner"`
	Description  string       `json:"description"`
	StorageProof StorageProof `json:"storage_proof"`
}

func (tx TxCreateContract) TxSelf() TxSelf {
	return tx.Self
}

const CONTRACT_DATA_AVAILABLITY_PROOF_REQUIRED_HEIGHT = 84162592

// ProcessTx implements VSCTransaction.
func (tx TxCreateContract) ExecuteTx(se *StateEngine) {
	if tx.Self.BlockHeight > CONTRACT_DATA_AVAILABLITY_PROOF_REQUIRED_HEIGHT {
		fmt.Println("Must validate storage proof")
		// tx.StorageProof.
		election, err := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight)

		if err != nil {
			// panic("disabled")
			return
		}

		verified := tx.StorageProof.Verify(election)

		fmt.Println("Storage proof verify result", verified)

		// panic("not implemented yet")
	}

	fmt.Println("tx.Code", tx)
	cid := cid.MustParse(tx.Code)
	go func() {
		se.da.GetDag(cid)
	}()

	idObj := map[string]interface{}{
		"ref_id": tx.Self.TxId,
		"index":  strconv.Itoa(tx.Self.OpIndex),
	}
	contractIdDag, _ := dagCbor.WrapObject(idObj, mh.SHA2_256, -1)

	conv, _ := bech32.ConvertBits(contractIdDag.Cid().Bytes(), 8, 5, true)
	bech32Addr, _ := bech32.Encode("vs4", conv)

	var owner string
	if tx.Owner == "" {
		owner = tx.Self.RequiredAuths[0]
	} else {
		owner = tx.Owner
	}

	se.contractDb.RegisterContract(bech32Addr, contracts.SetContractArgs{
		Code:           tx.Code,
		Name:           tx.Name,
		Description:    tx.Description,
		Creator:        tx.Self.RequiredAuths[0],
		Owner:          owner,
		TxId:           tx.Self.TxId,
		CreationHeight: tx.Self.BlockHeight,
	})

	// dd := map[string]interface{}{
	// 	"bytes": []byte("HELLO WORLD LOLLL"),
	// }
	// dagCbor, _ := dagCbor.WrapObject(dd, mh.SHA2_256, -2)

	// cid2, _ := se.da.PutObject(dd)
	// bbytes, _ := dagCbor.MarshalJSON()
	// fmt.Println("GDAGCBOR TEST", string(bbytes), cid2)

}

type StorageProof struct {
	Hash      string                 `json:"hash"`
	Signature dids.SerializedCircuit `json:"signature"`
}

// TODO: Define everything else that'll happen with this
func (sp *StorageProof) Verify(electionInfo elections.ElectionResult) bool {
	didMembers := make([]dids.BlsDID, 0)
	for _, v := range electionInfo.Members {
		didMembers = append(didMembers, dids.BlsDID(v.Key))
	}
	cid, err := cid.Parse(sp.Hash)

	if err != nil {
		return false
	}
	circuit, err := dids.DeserializeBlsCircuit(sp.Signature, didMembers, cid)

	if err != nil {
		return false
	}
	verified, includedDids, err := circuit.Verify()

	if !verified || err != nil || len(includedDids) < 2 {
		return false
	}

	return true
}

type TxElectionResult struct {
	Self TxSelf

	BlockHeight uint64
	Data        string                 `json:"data"`
	Epoch       uint64                 `json:"epoch"`
	NetId       string                 `json:"net_id"`
	Signature   dids.SerializedCircuit `json:"signature"`
}

func (tx TxElectionResult) TxSelf() TxSelf {
	return tx.Self
}

// ProcessTx implements VSCTransaction.
func (tx TxElectionResult) ExecuteTx(se *StateEngine) {
	// ctx := context.Background()
	if tx.Epoch == 0 {
		electionResult := se.electionDb.GetElection(0)

		if electionResult == nil {
			parsedCid, err := cid.Parse(tx.Data)
			// fmt.Println("Cid data", tx.Data, err, "Tx data", tx)
			if err != nil {
				return
			}
			// fmt.Println("Hit here 2")
			node, _ := se.da.Get(parsedCid, nil)

			dagNode, _ := dagCbor.Decode((*node).RawData(), mh.SHA2_256, -1)
			elecResult := elections.ElectionResult{}
			bbytes, _ := dagNode.MarshalJSON()
			json.Unmarshal(bbytes, &elecResult)

			elecResult.Proposer = tx.Self.RequiredAuths[0]
			elecResult.BlockHeight = tx.Self.BlockHeight
			elecResult.Epoch = tx.Epoch
			elecResult.NetId = tx.NetId
			elecResult.Data = tx.Data

			// fmt.Println("TxElection bytes", string(bbytes))
			// fmt.Println("Hit here 3")
			//Store
			se.electionDb.StoreElection(elecResult)
		}
	} else {
		//Validate normally
		prevElection := se.electionDb.GetElection(tx.Epoch - 1)
		if prevElection == nil {
			fmt.Println("NO PREVIOUS ELECTION")
			return
		}

		if prevElection.Epoch >= tx.Epoch {
			fmt.Println("Election is back in time!")
			return
		}

		//Weight that is calculated by aggregating number of signers
		potentialWeight := 0
		memberDids := make([]dids.BlsDID, 0)
		for idx, value := range prevElection.Members {
			memberDids = append(memberDids, dids.BlsDID(value.Key))

			if len(prevElection.Weights) == 0 {
				potentialWeight += 1
			} else {
				potentialWeight = int(prevElection.Weights[idx])
			}
		}

		verifyObj := map[string]interface{}{
			"data":   tx.Data,
			"epoch":  tx.Epoch,
			"net_id": tx.NetId,
		}
		verifyHash, _ := dagCbor.WrapObject(verifyObj, mh.SHA2_256, -1)

		parsedCid, _ := cid.Parse(tx.Data)

		blsCircuit, err := dids.DeserializeBlsCircuit(tx.Signature, memberDids, verifyHash.Cid())

		if err != nil {
			return
		}

		verified, includedDids, err := blsCircuit.Verify()

		totalWeight := uint64(0)
		if len(prevElection.Weights) == 0 {
			totalWeight = uint64(len(prevElection.Members))
		} else {
			for _, v := range prevElection.Weights {
				totalWeight = totalWeight + v
			}
		}

		blocksLastElection := tx.Self.BlockHeight - prevElection.BlockHeight

		minimums := elections.MinimalRequiredElectionVotes(blocksLastElection, totalWeight)

		realWeight := uint64(0)
		bv := blsCircuit.RawBitVector()
		for idx := range prevElection.Members {
			if bv.Bit(idx) == 1 {
				if len(prevElection.Weights) == 0 {
					realWeight += 1
				} else {
					realWeight += prevElection.Weights[idx]
				}
			}
		}
		fmt.Println("Minimum requirements", minimums, "orig reqs", len(prevElection.Members)*2/3)

		fmt.Println("realWeight", realWeight, " len(includedDids)", len(includedDids))

		if verified && realWeight >= minimums {
			fmt.Println("Election verified, indexing...", tx.Epoch)
			fmt.Println("Election CID", parsedCid)
			se.da.GetDag(parsedCid)
			fmt.Println("Got dag prolly")
			node, _ := se.da.Get(parsedCid, nil)
			fmt.Println("Got Election from DA")
			//Verified and 2/3 majority signed
			dagNode, _ := dagCbor.Decode((*node).RawData(), mh.SHA2_256, -1)
			elecResult := elections.ElectionResult{
				Proposer:    tx.Self.RequiredAuths[0],
				BlockHeight: tx.Self.BlockHeight,
			}
			elecResult.Epoch = tx.Epoch
			elecResult.NetId = tx.NetId
			elecResult.Data = tx.Data

			bbytes, _ := dagNode.MarshalJSON()
			json.Unmarshal(bbytes, &elecResult)

			se.electionDb.StoreElection(elecResult)
			fmt.Println("Indexed Election", tx.Epoch)
		} else {
			fmt.Println("Election Failed verification")
		}

	}
}
