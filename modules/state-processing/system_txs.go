package state_engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	transactionpool "vsc-node/modules/transaction-pool"
	wasm_runtime "vsc-node/modules/wasm/runtime"

	"github.com/ipfs/go-cid"
	dagCbor "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"
	"go.mongodb.org/mongo-driver/mongo"
)

type ContractOutput struct {
	Id         string                     `json:"id"`
	ContractId string                     `json:"contract_id"`
	Inputs     []string                   `json:"inputs"`
	Metadata   contracts.ContractMetadata `json:"metadata"`
	//This might not be used

	Results     []contracts.ContractOutputResult `json:"results" bson:"results"`
	StateMerkle string                           `json:"state_merkle"`

	// Legacy, moved to results array
	TssOps []tss_db.TssOp `json:"tss_ops"`
}

func (output *ContractOutput) Ingest(se *StateEngine, txSelf TxSelf, slotHeight int64) {
	se.Flush()

	txOuts := make(map[string][]int)

	for idx, inputId := range output.Inputs {
		inputTxId := strings.Split(inputId, "-")[0]
		txOuts[inputTxId] = append(txOuts[inputTxId], idx)
	}

	for txId, txOutIdxs := range txOuts {
		se.txDb.SetOutput(transactions.SetResultUpdate{
			Id: txId,
			Output: &transactions.TransactionOutput{
				Id:    output.Id,
				Index: txOutIdxs,
			},
		})
	}

	tssOps := output.TssOps
	for _, res := range output.Results {
		tssOps = append(tssOps, res.TssOps...)
	}

	for _, tssOp := range tssOps {
		if tssOp.Type == "create" {
			fmt.Println("CREATING TSS KEY", tssOp)
			_, err := se.tssKeys.FindKey(tssOp.KeyId)

			// fmt.Println("err", err)
			if err == mongo.ErrNoDocuments {
				se.tssKeys.InsertKey(tssOp.KeyId, tss_db.TssKeyAlgo(tssOp.Args))
			}
		} else if tssOp.Type == "sign" {
			se.tssRequests.SetSignedRequest(tss_db.TssRequest{
				KeyId:  tssOp.KeyId,
				Status: "unsigned",
				Msg:    tssOp.Args,
			})
			// if err == mongo.ErrNoDocuments {
			// 	se.tssKeys.InsertKey(tssOp.KeyId, tss_db.TssKeyAlgo(tssOp.Args))
			// }
		}
	}

	go func() {
		cid, err := cid.Parse(output.StateMerkle)
		if err == nil {
			db := datalayer.NewDataBinFromCid(se.da, cid)
			list, _ := db.List("")
			if list != nil {
				for _, v := range *list {
					cidz, err := db.Get(v)
					if err == nil {
						se.da.Get(*cidz, &common_types.GetOptions{})
					}
				}
			}
		}
	}()
	//Set output history
	se.contractState.IngestOutput(contracts.IngestOutputArgs{
		Id:         output.Id,
		ContractId: output.ContractId,

		Metadata:    output.Metadata,
		StateMerkle: output.StateMerkle,
		Inputs:      output.Inputs,
		Results:     output.Results,

		AnchoredBlock:  txSelf.BlockId,
		AnchoredHeight: slotHeight,
		AnchoredId:     txSelf.TxId,
		AnchoredIndex:  int64(txSelf.Index),
	})
}

type TxCreateContract struct {
	Self TxSelf `json:"-"`

	Version      string               `json:"__v"`
	NetId        string               `json:"net_id"`
	Name         string               `json:"name"`
	Code         string               `json:"code"`
	Runtime      wasm_runtime.Runtime `json:"runtime"`
	Owner        string               `json:"owner"`
	Description  string               `json:"description"`
	StorageProof StorageProof         `json:"storage_proof"`
}

func (tx TxCreateContract) Type() string {
	return "create_contract"
}

func (tx TxCreateContract) TxSelf() TxSelf {
	return tx.Self
}

const CONTRACT_DATA_AVAILABLITY_PROOF_REQUIRED_HEIGHT = 84162592

// ProcessTx implements VSCTransaction.
func (tx *TxCreateContract) ExecuteTx(se *StateEngine) TxResult {

	// fmt.Println("tx.Runtime", tx.Runtime)
	if wasm_runtime.NewFromString(tx.Runtime.String()).IsErr() {
		return TxResult{
			Success: false,
			Ret:     "runtime name is invalid",
		}
	}

	if len(tx.Self.RequiredAuths) == 0 {
		return TxResult{
			Success: false,
			Ret:     "cannot create contract with posting auths",
		}
	}

	// fmt.Println("Must validate storage proof")

	election, err := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight)

	if err != nil {
		panic("Failed to get election")
	}

	verified := tx.StorageProof.Verify(election)

	// fmt.Println("Storage proof verify result", verified)

	if !verified {
		return TxResult{
			Success: false,
			Ret:     "invalid storage proof",
		}
	}

	cidz := cid.MustParse(tx.Code)
	go func() {
		se.da.Get(cidz, &common_types.GetOptions{})
	}()

	id := common.ContractId(tx.Self.TxId, tx.Self.OpIndex)

	var owner string
	if tx.Owner == "" {
		owner = tx.Self.RequiredAuths[0]
	} else {
		owner = tx.Owner
		if !strings.HasPrefix(owner, "hive:") && !strings.HasPrefix(owner, "did:") {
			owner = "hive:" + owner
		}
	}

	se.contractDb.RegisterContract(id, contracts.Contract{
		Code:           tx.Code,
		Name:           tx.Name,
		Description:    tx.Description,
		Creator:        tx.Self.RequiredAuths[0],
		Owner:          owner,
		TxId:           tx.Self.TxId,
		CreationHeight: tx.Self.BlockHeight,
		Runtime:        tx.Runtime,
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
		"runtime":       tx.Runtime,
	}
}

type StorageProof struct {
	Hash      string                 `json:"hash"`
	Signature dids.SerializedCircuit `json:"signature"`
}

const STORAGE_PROOF_MINIMUM_SIGNERS = 6

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

	if !verified || err != nil || len(includedDids) < STORAGE_PROOF_MINIMUM_SIGNERS {
		return false
	}

	return true
}

type TxUpdateContract struct {
	Self         TxSelf                `json:"-"`
	NetId        string                `json:"net_id"`
	Id           string                `json:"id"`
	Name         string                `json:"name"`
	Description  string                `json:"description"`
	Owner        string                `json:"owner,omitempty"`
	Runtime      *wasm_runtime.Runtime `json:"runtime,omitempty"`
	Code         string                `json:"code,omitempty"`
	StorageProof *StorageProof         `json:"storage_proof,omitempty"`
}

type UpdateContractResult struct {
	Success     bool
	CodeUpdated bool
	Err         string
}

func (tx TxUpdateContract) Type() string {
	return "update_contract"
}

func (tx TxUpdateContract) TxSelf() TxSelf {
	return tx.Self
}

func (tx *TxUpdateContract) ToData() map[string]interface{} {
	return map[string]interface{}{
		"net_id":        tx.NetId,
		"id":            tx.Id,
		"name":          tx.Name,
		"description":   tx.Description,
		"owner":         tx.Owner,
		"runtime":       tx.Runtime,
		"code":          tx.Code,
		"storage_proof": tx.StorageProof,
	}
}

func (tx *TxUpdateContract) ExecuteTx(se *StateEngine, hasFee bool) UpdateContractResult {
	if len(tx.Self.RequiredAuths) == 0 {
		return UpdateContractResult{
			Success: false,
			Err:     "cannot update contract with posting auths",
		}
	}
	existing, err := se.contractDb.ContractById(tx.Id, tx.Self.BlockHeight)
	if err != nil {
		return UpdateContractResult{
			Success: false,
			Err:     "failed to retrieve contract to update",
		}
	}
	if tx.Self.RequiredAuths[0] != existing.Owner {
		return UpdateContractResult{
			Success: false,
			Err:     "not owner",
		}
	}
	updatedContract := contracts.Contract{
		Name:        tx.Name,
		Description: tx.Description,
		Creator:     existing.Creator,
		Owner:       existing.Owner,
		Runtime:     existing.Runtime,
		Code:        existing.Code,

		// contract update history
		TxId:           tx.Self.TxId,
		CreationHeight: tx.Self.BlockHeight,
	}
	if tx.Owner != "" {
		// update owner
		if !strings.HasPrefix(tx.Owner, "hive:") && !strings.HasPrefix(tx.Owner, "did:") {
			updatedContract.Owner = "hive:" + tx.Owner
		} else {
			updatedContract.Owner = tx.Owner
		}
	}
	if hasFee && tx.Code != "" && tx.Code != existing.Code {
		// update contract code
		if wasm_runtime.NewFromString(tx.Runtime.String()).IsErr() {
			return UpdateContractResult{
				Success: false,
				Err:     "runtime name is invalid",
			}
		}
		election, err := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight)
		if err != nil {
			return UpdateContractResult{
				Success: false,
				Err:     "failed to get election",
			}
		}
		verified := tx.StorageProof.Verify(election)
		if !verified {
			return UpdateContractResult{
				Success: false,
				Err:     "invalid storage proof",
			}
		}
		cidz := cid.MustParse(tx.Code)
		go func() {
			se.da.Get(cidz, &common_types.GetOptions{})
		}()
		updatedContract.Code = tx.Code
		updatedContract.Runtime = *tx.Runtime
	}
	se.contractDb.RegisterContract(tx.Id, updatedContract)

	return UpdateContractResult{
		Success:     true,
		CodeUpdated: updatedContract.Code != existing.Code,
	}
}

type TxElectionResult struct {
	Self TxSelf

	BlockHeight uint64
	Data        string                 `json:"data"`
	Epoch       uint64                 `json:"epoch"`
	NetId       string                 `json:"net_id"`
	EType       string                 `json:"type"`
	Signature   dids.SerializedCircuit `json:"signature"`
}

func (tx TxElectionResult) Type() string {
	return "election_result"
}

func (tx TxElectionResult) TxSelf() TxSelf {
	return tx.Self
}

// ProcessTx implements VSCTransaction.
func (tx *TxElectionResult) ExecuteTx(se *StateEngine) {
	// ctx := context.Background()
	if tx.Epoch == 0 {
		electionResult := se.electionDb.GetElection(0)

		if electionResult == nil {
			parsedCid, err := cid.Parse(tx.Data)
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
			elecResult.TxId = tx.Self.TxId
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
			"__t":    "approve_election",
			"data":   tx.Data,
			"epoch":  tx.Epoch,
			"net_id": tx.NetId,
			"type":   tx.EType,
		}
		verifyData, _ := common.EncodeDagCbor(verifyObj)

		verifyHash, err := cid.Prefix{
			Version:  1,
			Codec:    uint64(multicodec.DagCbor),
			MhType:   mh.SHA2_256,
			MhLength: -1,
		}.Sum(verifyData)

		if err != nil {
			fmt.Println("Failed to create cid for election", err)
			return
		}

		parsedCid, _ := cid.Parse(tx.Data)

		blsCircuit, err := dids.DeserializeBlsCircuit(tx.Signature, memberDids, verifyHash)
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
		fmt.Println(verified, "realWeight", realWeight, " len(includedDids)", len(includedDids))

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
				TxId:        tx.Self.TxId,
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

	Signers []string `json:"-"`
	Epoch   uint64   `json:"-"`
}

func (tx *TxProposeBlock) Type() string {
	return "propose_block"
}

func (tx *TxProposeBlock) TxSelf() TxSelf {
	return tx.Self
}

func (t *TxProposeBlock) Validate(se *StateEngine) bool {
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

	verified, includedDids, err := circuit.Verify()

	// fmt.Println("circuit.Verify()", err)

	if uint64(t.SignedBlock.Headers.Br[1])+CONSENSUS_SPECS.SlotLength <= t.Self.BlockHeight {
		fmt.Println("Block is too far in the future", t.SignedBlock.Headers.Br, uint64(t.SignedBlock.Headers.Br[1])+CONSENSUS_SPECS.SlotLength, t.Self.BlockHeight)
		return false
	}

	// fmt.Println("Verified sig", verified)
	if !verified {
		return false
	}

	signingScore, total := elections.CalculateSigningScore(*circuit, elecResult)
	// fmt.Println("signingScore, total", signingScore, total, signingScore > ((total*2)/3))
	//PASS
	// blockCid := cid.MustParse(t.SignedBlock.Block)

	verifiedR := signingScore > ((total * 2) / 3)

	for _, did := range includedDids {
		for _, member := range elecResult.Members {
			if did.String() == member.Key {
				t.Signers = append(t.Signers, member.Account)
			}
		}
	}
	t.Epoch = elecResult.Epoch

	return verifiedR
}

// ProcessTx implements VSCTransaction.
func (t *TxProposeBlock) ExecuteTx(se *StateEngine) {

	blockCid, _ := cid.Parse(t.SignedBlock.Block)
	node, _ := se.da.GetDag(blockCid)
	jsonBytes, _ := node.MarshalJSON()
	blockContentC := vscBlocks.VscBlock{}
	// json.Unmarshal(jsonBytes, &blockContent)

	se.da.GetObject(blockCid, &blockContentC, common_types.GetOptions{})

	slotInfo := CalculateSlotInfo(t.Self.BlockHeight)

	se.vscBlocks.StoreHeader(vscBlocks.VscHeaderRecord{
		Id: t.Self.TxId,

		MerkleRoot: blockContentC.MerkleRoot,
		Proposer:   t.Self.RequiredAuths[0],
		SigRoot:    blockContentC.SigRoot,

		EndBlock:     t.SignedBlock.Headers.Br[1],
		StartBlock:   t.SignedBlock.Headers.Br[0] + 1,
		BlockContent: t.SignedBlock.Block,

		// SlotHeight: ,

		Signers:    t.Signers,
		Epoch:      t.Epoch,
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

	nonceUpdates := make(map[string]uint64)

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
			Index:       idx,
			BlockHeight: uint64(t.SignedBlock.Headers.Br[1]),
			BlockId:     t.Self.BlockId,
		})

		if txContainer.Type() == "transaction" {
			//Note: sig verification has already happened
			tx := txContainer.AsTransaction()

			tx.Ingest(se, t.Self.TxId, TxSelf{
				BlockId:     t.Self.BlockId,
				BlockHeight: uint64(t.SignedBlock.Headers.Br[1]),
				//
				Index: idx,
			})

			// fmt.Println("broadcast inject tx", tx.Headers.Nonce, tx.Headers.RequiredAuths)
			keyId := transactionpool.HashKeyAuths(tx.Headers.RequiredAuths)
			if nonceUpdates[keyId] < tx.Headers.Nonce || nonceUpdates[keyId] == 0 {
				nonceUpdates[keyId] = tx.Headers.Nonce
			}

			txs := tx.ToTransaction()
			txsToInjest = append(txsToInjest, TxPacket{
				TxId: tx.Cid().String(),
				Ops:  txs,
			})
		} else if txContainer.Type() == "output" {
			contractOutput := txContainer.AsContractOutput()
			// fmt.Println(contractOutput, string(jsonBlsaz))

			contractOutput.Ingest(se, TxSelf{
				BlockId:     t.Self.BlockId,
				BlockHeight: t.Self.BlockHeight,
				TxId:        t.Self.TxId,
			}, int64(se.slotStatus.SlotHeight))

			fmt.Println("OUTPUT CONTAINER", contractOutput)

		} else if txContainer.Type() == "oplog" {
			oplog := txContainer.AsOplog(uint64(t.SignedBlock.Headers.Br[1]))
			// fmt.Println("OpLog detected!", txContainer, oplog)
			oplog.ExecuteTx(se)
		}
	}

	for k, v := range nonceUpdates {
		se.nonceDb.SetNonce(k, v+1)
	}

	// for _, v := range txsToInjest {
	// 	se.txDb.Ingest(

	// 	)
	// }

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
