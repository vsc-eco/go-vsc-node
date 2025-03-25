package stateEngine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"

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

func (tx TxCreateContract) Type() string {
	return "create_contract"
}

func (tx TxCreateContract) TxSelf() TxSelf {
	return tx.Self
}

const CONTRACT_DATA_AVAILABLITY_PROOF_REQUIRED_HEIGHT = 84162592

// ProcessTx implements VSCTransaction.
func (tx *TxCreateContract) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession) TxResult {
	fmt.Println("Must validate storage proof")
	// tx.StorageProof.
	election, err := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight)

	if err != nil {
		panic("Failed to get election")
	}

	verified := tx.StorageProof.Verify(election)

	fmt.Println("Storage proof verify result", verified)

	// panic("not implemented yet")

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

	return TxResult{
		Success: true,
	}
}

func (tx *TxCreateContract) ToData() map[string]interface{} {
	return map[string]interface{}{
		"__v":           tx.Version,
		"net_id":        tx.NetId,
		"name":          tx.Name,
		"code":          tx.Code,
		"owner":         tx.Owner,
		"description":   tx.Description,
		"storage_proof": tx.StorageProof,
	}
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

func (tx TxElectionResult) Type() string {
	return "election_result"
}

func (tx TxElectionResult) TxSelf() TxSelf {
	return tx.Self
}

// ProcessTx implements VSCTransaction.
func (tx *TxElectionResult) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession) {
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

			dagNode, _ := dagCbor.Decode(node.RawData(), mh.SHA2_256, -1)
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
			dagNode, _ := dagCbor.Decode(node.RawData(), mh.SHA2_256, -1)
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

func (tx *TxElectionResult) ToData() map[string]interface{} {

	return map[string]interface{}{
		"epoch":     tx.Epoch,
		"net_id":    tx.NetId,
		"data":      tx.Data,
		"signature": tx.Signature,
	}
}

type TxProposeBlock struct {
	Self TxSelf

	//ReplayId should be deprecated soon
	NetId       string            `json:"net_id"`
	SignedBlock SignedBlockHeader `json:"signed_block"`
}

func (tx TxProposeBlock) Type() string {
	return "propose_block"
}

func (tx TxProposeBlock) TxSelf() TxSelf {
	return tx.Self
}

func (t TxProposeBlock) Validate(se *StateEngine) bool {
	elecResult, err := se.electionDb.GetElectionByHeight(t.Self.BlockHeight)
	if err != nil {
		//Cannot process block due to missing election
		return false
	}
	memberDids := make([]dids.BlsDID, 0)
	for _, member := range elecResult.Members {
		memberDids = append(memberDids, dids.BlsDID(member.Key))
	}

	//We can't use json convert then unmarshell due to CID instance must be passed to cbor lib
	//..to properly serialize the CID into the correct cbor type
	// blockHeader := map[string]interface{}{
	// 	"__v": t.SignedBlock.Version,
	// 	"__t": t.SignedBlock.Type,
	// 	"headers": map[string]interface{}{
	// 		"br":    t.SignedBlock.Headers.Br,
	// 		"prevb": t.SignedBlock.Headers.PrevBlock,
	// 	},
	// 	"merkle_root": t.SignedBlock.MerkleRoot,
	// 	"block":       t.SignedBlock.Block,
	// }

	blockCid, _ := cid.Parse(t.SignedBlock.Block)
	blockHeader := vscBlocks.VscHeader{
		Type:    t.SignedBlock.Type,
		Version: t.SignedBlock.Version,
		Headers: struct {
			Br    [2]int  "refmt:\"br\""
			Prevb *string "refmt:\"prevb\""
		}{
			Br:    t.SignedBlock.Headers.Br,
			Prevb: t.SignedBlock.Headers.PrevBlock,
		},
		MerkleRoot: t.SignedBlock.MerkleRoot,
		Block:      blockCid,
	}

	// dag, _ := dagCbor.WrapObject(blockHeader, mh.SHA2_256, -1)
	cid, _ := se.da.HashObject(blockHeader)

	// fmt.Println("Validated CID", cid)
	// fmt.Println("MemberDids", memberDids)
	circuit, err := dids.DeserializeBlsCircuit(t.SignedBlock.Signature, memberDids, *cid)

	verified, _, err := circuit.Verify()

	// fmt.Println("circuit.Verify()", err)

	if uint64(t.SignedBlock.Headers.Br[1])+CONSENSUS_SPECS.SlotLength <= t.Self.BlockHeight {
		fmt.Println("Block is too far in the future", t.SignedBlock.Headers.Br, uint64(t.SignedBlock.Headers.Br[1])+CONSENSUS_SPECS.SlotLength, t.Self.BlockHeight)
		return false
	}

	fmt.Println("Verified sig", verified)
	if !verified {
		return false
	}

	signingScore, total := elections.CalculateSigningScore(*circuit, elecResult)
	fmt.Println("signingScore, total", signingScore, total, signingScore > ((total*2)/3))
	//PASS
	// blockCid := cid.MustParse(t.SignedBlock.Block)

	verifiedR := signingScore > ((total * 2) / 3)

	return verifiedR
}

// ProcessTx implements VSCTransaction.
func (t *TxProposeBlock) ExecuteTx(se *StateEngine) {
	blockCid, _ := cid.Parse(t.SignedBlock.Block)
	node, _ := se.da.GetDag(blockCid)
	jsonBytes, _ := node.MarshalJSON()
	blockContentC := vscBlocks.VscBlock{}
	// json.Unmarshal(jsonBytes, &blockContent)

	se.da.GetObject(blockCid, &blockContentC, datalayer.GetOptions{})

	// fmt.Println("399 err GetObject", err, blockContentC)

	// bbyes, _ := json.Marshal(blockContentC)
	// fmt.Println("Decoded VSC Block header", string(bbyes))

	slotInfo := CalculateSlotInfo(t.Self.BlockHeight)

	se.vscBlocks.StoreHeader(vscBlocks.VscHeaderRecord{
		Id: t.Self.TxId,

		MerkleRoot: blockContentC.MerkleRoot,
		Proposer:   t.Self.RequiredAuths[0],
		SigRoot:    blockContentC.SigRoot,

		// SlotHeight: ,

		SlotHeight: int(slotInfo.StartHeight),
		Stats: struct {
			Size uint64 `bson:"size"`
		}{
			Size: uint64(len(jsonBytes)),
		},
		Ts:        t.Self.Timestamp,
		DebugData: blockContentC,
	})

	txsToInjest := make([]TxPacket, 0)

	//At this point of the process a call should be made to state engine
	//To kick off finalization of the inflight state
	//Such as transfers, contract calls, etc
	//New TXs should be indexed at this point
	for idx, txInfo := range blockContentC.Transactions {
		tx := BlockTx{
			Id:   txInfo.Id,
			Op:   txInfo.Op,
			Type: txInfo.Type,
		}
		//Things to Process
		// - Contract executionll
		// - Transfers, withdraws
		// - New TXs (repeat process in state engine)
		//Note: VSC txs can be processed immediately once anchored on chain
		//Thus: TX confirmation is 30s maximum
		//Author: @vaultec81

		txContainer := tx.Decode(se.da, TxSelf{
			TxId:        txInfo.Id,
			Index:       -1,
			OpIndex:     idx,
			BlockHeight: uint64(t.SignedBlock.Headers.Br[1]),
			BlockId:     t.Self.BlockId,
		})

		if txContainer.Type() == "transaction" {
			//Note: sig verification has already happened
			tx := txContainer.AsTransaction()

			tx.Ingest(se, TxSelf{
				BlockId:     t.Self.BlockId,
				BlockHeight: uint64(t.SignedBlock.Headers.Br[1]),
				//
				Index:   -1,
				OpIndex: idx,
			})

			txs := tx.ToTransaction()
			txsToInjest = append(txsToInjest, TxPacket{
				TxId: t.Self.TxId,
				Ops:  txs,
			})
		} else if txContainer.Type() == "output" {
			contractOutput := txContainer.AsContractOutput()
			// fmt.Println(contractOutput, string(jsonBlsaz))

			contractOutput.Ingest(se, TxSelf{
				BlockId:     t.Self.BlockId,
				BlockHeight: t.Self.BlockHeight,
				TxId:        t.Self.TxId,
			})

		} else if txContainer.Type() == "oplog" {

			oplog := txContainer.AsOplog()
			fmt.Println("OpLog detected!", txContainer, oplog)
			oplog.ExecuteTx(se)
		}
	}
	se.TxBatch = append(txsToInjest, se.TxBatch...)
}

type SignedBlockHeader struct {
	UnsignedBlockHeader
	Signature dids.SerializedCircuit `json:"signature"`
}

type UnsignedBlockHeader struct {
	Type    string `json:"__t"`
	Version string `json:"__v"`
	Headers struct {
		PrevBlock *string `json:"prevb"`
		Br        [2]int  `json:"br"`
	} `json:"headers"`
	//Define a potential struct to streamline merkle proofs.
	//Maybe convert to that struct too
	MerkleRoot *string `json:"merkle_root"`
	Block      string  `json:"block"`
}

type BlockContent struct {
	Headers struct {
		PrevBlock string `json:"prevb"`
	} `json:"headers"`
	Transactions []BlockTx `json:"txs"`

	//Maybe in future make a magic merkle root class with a prototype of string..
	//..and functions to do proof verification
	MerkleRoot string `json:"merkle_root"`
	SigRoot    string `json:"sig_root"`
}

// Reference pointer to the transaction itself
type BlockTx struct {
	Id string  `json:"id"`
	Op *string `json:"op,omitempty"`
	// 1 input
	// 2 output
	// 5 anchor
	// 6 oplog

	Type int `json:"type"`
}

func (bTx *BlockTx) Decode(da *datalayer.DataLayer, txSelf TxSelf) TransactionContainer {
	//Do some conversion back to a TX type?
	txCid := cid.MustParse(bTx.Id)

	dagNode, _ := da.GetDag(txCid)
	tx := TransactionContainer{
		da:      da,
		Id:      bTx.Id,
		TypeInt: bTx.Type,

		Self: txSelf,
	}
	tx.Decode(dagNode.RawData())

	return tx
}
