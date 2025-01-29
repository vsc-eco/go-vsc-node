package stateEngine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/transactions"

	"github.com/btcsuite/btcutil/bech32"
	blocks "github.com/ipfs/go-block-format"

	"github.com/ipfs/go-cid"
	dagCbor "github.com/ipfs/go-ipld-cbor"
	mh "github.com/multiformats/go-multihash"
)

type CustomJson struct {
	Id                   string   `json:"id"`
	RequiredAuths        []string `json:"required_auths"`
	RequiredPostingAuths []string `json:"required_posting_auths"`
	Json                 []byte   `json:"json"`
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

const CONTRACT_DATA_AVAILABLITY_PROOF_REQUIRED_HEIGHT = 84162592

// ProcessTx implements VSCTransaction.
func (tx TxCreateContract) ExecuteTx(se *StateEngine) {
	if tx.Self.BlockHeight > CONTRACT_DATA_AVAILABLITY_PROOF_REQUIRED_HEIGHT {
		fmt.Println("Must validate storage proof")
		// tx.StorageProof.
		election := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight)

		if election != nil {
			// panic("disabled")
			return
		}

		verified := tx.StorageProof.Verify(*election)

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

// ProcessTx implements VSCTransaction.
func (tx TxElectionResult) ExecuteTx(se *StateEngine) {
	if tx.Self.BlockHeight < 83313113 {
		return
	}
	// ctx := context.Background()
	if tx.Epoch == 0 {
		electionResult := se.electionDb.GetElection(0)

		if electionResult == nil {
			parsedCid, err := cid.Parse(tx.Data)
			fmt.Println("Cid data", tx.Data, err, "Tx data", tx)
			if err != nil {
				// return
			}
			fmt.Println("Hit here 2")
			node, _ := se.da.Get(parsedCid, nil)

			dagNode, _ := dagCbor.Decode((*node).RawData(), mh.SHA2_256, -1)
			elecResult := elections.ElectionResult{}
			bbytes, _ := dagNode.MarshalJSON()
			json.Unmarshal(bbytes, &elecResult)

			fmt.Println("Hit here 3")
			//Store
			se.electionDb.StoreElection(elections.ElectionResult{
				Proposer:    tx.Self.RequiredAuths[0],
				BlockHeight: tx.Self.BlockHeight,
				Epoch:       tx.Epoch,
				NetId:       tx.NetId,
				Data:        tx.Data,
				Members:     elecResult.Members,
			})
		} else {
			fmt.Println("ln 180")
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
			if prevElection.WeightTotal == 0 {
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
		fmt.Println("verifyObj", verifyObj, verifyHash.Cid())

		parsedCid, _ := cid.Parse(tx.Data)

		blsCircuit, err := dids.DeserializeBlsCircuit(tx.Signature, memberDids, verifyHash.Cid())

		fmt.Println("Err deserialize", err)

		fmt.Println("IncludedDids", blsCircuit.IncludedDIDs())

		verified, includedDids, err := blsCircuit.Verify()

		fmt.Println("Verify error", err, verified, len(includedDids) > (len(memberDids)*2/3))

		totalWeight := uint64(0)
		if prevElection.WeightTotal == 0 {
			totalWeight = uint64(len(prevElection.Members))
		} else {
			totalWeight = prevElection.WeightTotal
		}

		blocksLastElection := tx.Self.BlockHeight - prevElection.BlockHeight

		fmt.Println("Blocks since last election!", blocksLastElection)
		minimums := elections.MinimalRequiredElectionVotes(blocksLastElection, totalWeight)

		realWeight := 0
		bv := blsCircuit.RawBitVector()
		for idx := range prevElection.Members {
			if bv.Bit(idx) == 1 {
				if prevElection.WeightTotal == 0 {
					realWeight += 1
				} else {
					realWeight += int(prevElection.Weights[idx])
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
				Epoch:       tx.Epoch,
				NetId:       tx.NetId,
				Data:        tx.Data,
			}
			bbytes, _ := dagNode.MarshalJSON()
			json.Unmarshal(bbytes, &elecResult)

			se.electionDb.StoreElection(elecResult)
			fmt.Println("Indexed Election", tx.Epoch)
		} else {
			fmt.Println("Election Failed verification")
		}

		// if prevElection.Epoch > 124 {
		// 	fmt.Println("HELL ON EARTH")
		// 	time.Sleep(time.Hour)
		// }

		// fmt.Println("Cid data", tx.Data, err, "Tx data", tx)
		// if err != nil {
		// 	// return
		// }
		// fmt.Println("Hit here 2")
		// node, _ := se.da.Get(parsedCid, nil)

		// dagNode, _ := dagCbor.Decode((*node).RawData(), mh.SHA2_256, -1)
		// elecResult := elections.ElectionResult{}
		// bbytes, _ := dagNode.MarshalJSON()
		// json.Unmarshal(bbytes, &elecResult)

		// prevElection.Members

		fmt.Println("prevElection", prevElection)
	}
}

type TxProposeBlock struct {
	Self TxSelf

	//ReplayId should be deprecated soon
	ReplayId    int               `json:"replay_id"`
	NetId       string            `json:"net_id"`
	SignedBlock SignedBlockHeader `json:"signed_block"`
}

// ProcessTx implements VSCTransaction.
func (t TxProposeBlock) ExecuteTx(se *StateEngine) {
	elecResult := se.electionDb.GetElectionByHeight(t.Self.BlockHeight)
	fmt.Println("Election Epoch", elecResult.Epoch)
	if elecResult == nil {
		//Cannot process block due to missing election
		return
	}
	fmt.Println("Proposer!", elecResult.Proposer)
	memberDids := make([]dids.BlsDID, 0)
	for _, member := range elecResult.Members {
		memberDids = append(memberDids, dids.BlsDID(member.Key))
	}

	//We can't use json convert then unmarshell due to CID instance must be passed to cbor lib
	//..to properly serialize the CID into the correct cbor type
	blockHeader := map[string]interface{}{
		"__v": t.SignedBlock.Version,
		"__t": t.SignedBlock.Type,
		"headers": map[string]interface{}{
			"br":    t.SignedBlock.Headers.Br,
			"prevb": t.SignedBlock.Headers.PrevBlock,
		},
		"merkle_root": t.SignedBlock.MerkleRoot,
		"block":       cid.MustParse(t.SignedBlock.Block),
	}
	dag, _ := dagCbor.WrapObject(blockHeader, mh.SHA2_256, -1)

	circuit, _ := dids.DeserializeBlsCircuit(t.SignedBlock.Signature, memberDids, dag.Cid())

	verified, _, _ := circuit.Verify()

	fmt.Println("br - block range", t.SignedBlock.Headers.Br, t.Self.BlockHeight)

	fmt.Println("Verified", verified)
	if t.SignedBlock.Headers.Br[1]+CONSENSUS_SPECS.SlotLength <= t.Self.BlockHeight {
		fmt.Println("Block is too far in the future")
		return
	}
	if verified {
		signingScore, total := elections.CalculateSigningScore(*circuit, *elecResult)
		fmt.Println("signingScore, total", signingScore, total, signingScore > ((total*2)/3))

		if signingScore > ((total * 2) / 3) {
			//PASS
			fmt.Println("Block Has passed inspection! Approved!")
			// blockCid := cid.MustParse(t.SignedBlock.Block)
			fmt.Println("Getting Block info!", t.SignedBlock.Block, t.Self.TxId)

			node, _ := se.da.GetDag(t.SignedBlock.Block)
			jsonBytes, _ := node.MarshalJSON()
			blockContent := BlockContent{}
			json.Unmarshal(jsonBytes, &blockContent)

			//At this point of the process a call should be made to state engine
			//To kick off finalization of the inflight state
			//Such as transfers, contract calls, etc
			//New TXs should be indexed at this point
			for idx, tx := range blockContent.Transactions {
				//Things to Process
				// - Contract execution
				// - Transfers, withdraws
				// - New TXs (repeat process in state engine)
				//Note: VSC txs can be processed immediately once anchored on chain
				//Thus: TX confirmation is 30s maximum
				//Author: @vaultec81

				if tx.Type == 2 {
					continue
				}

				fmt.Println("Decoding TX", tx.Id)

				txContainer := tx.Decode(se.da)
				fmt.Println(txContainer, txContainer.Type())

				if txContainer.Type() == "transaction" {
					//Note: sig verification has already happened
					tx := txContainer.AsTransaction()
					fmt.Println(tx)

					fmt.Println("Ingesting")
					tx.Ingest(se, TxSelf{
						BlockId:     t.Self.BlockId,
						BlockHeight: t.Self.BlockHeight,
						Index:       t.Self.Index,
						OpIndex:     idx,
					})

				} else if txContainer.Type() == "output" {
					contractOutput := txContainer.AsContractOutput()

					// jsonBlsaz, _ := json.Marshal(contractOutput)
					// fmt.Println(contractOutput, string(jsonBlsaz))

					contractOutput.Ingest(se, TxSelf{
						BlockId:     t.Self.BlockId,
						BlockHeight: t.Self.BlockHeight,
						TxId:        t.Self.TxId,
					})
				} else if txContainer.Type() == "events" {
					txContainer.AsEvents()
				}
			}
		}
	}

}

type SignedBlockHeader struct {
	UnsignedBlockHeader
	Signature dids.SerializedCircuit `json:"signature"`
}

type UnsignedBlockHeader struct {
	Type    string `json:"__t"`
	Version string `json:"__v"`
	Headers struct {
		PrevBlock string    `json:"prevb"`
		Br        [2]uint64 `json:"br"`
	} `json:"headers"`
	//Define a potential struct to streamline merkle proofs.
	//Maybe convert to that struct too
	MerkleRoot string  `json:"merkle_root"`
	Block      cid.Cid `json:"block"`
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
	Id string `json:"id"`
	Op string `json:"op"`
	// 1 input
	// 2 output
	// 5 anchor

	Type int `json:"type"`
}

func (bTx *BlockTx) Decode(da *datalayer.DataLayer) TransactionContainer {
	//Do some conversion back to a TX type?
	txCid := cid.MustParse(bTx.Id)

	dagNode, _ := da.GetDag(txCid)
	tx := TransactionContainer{
		da:      da,
		Id:      bTx.Id,
		TypeInt: bTx.Type,
	}
	tx.Decode(dagNode.RawData())

	return tx
}

// VSC interaction on Hive
type TxVscHive struct {
	Self TxSelf

	Type    string `json:"__t"`
	Version string `json:"__v"`
	NetId   string `json:"net_id"`
	//We don't have set type for this.
	Headers map[string]interface{} `json:"headers"`
	Tx      ITxBody                `json:"tx"`
}

// ProcessTx implements VSCTransaction.
func (t TxVscHive) ExecuteTx(se *StateEngine) {

}

type ITxBody interface {
	Type() string
	//Define
	AsContractCall() *TxVscContract
	AsTransfer() *TxVSCTransfer
	AsWithdraw() *TxVSCWithdraw
}

type TxVscContract struct {
	Op         string `json:"op"`
	Action     string `json:"action"`
	ContractId string `json:"contract_id"`
	Payload    string `json:"payload"`
}

type TxVSCTransfer struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amt"`
	Token  string `json:"tk"`
	Memo   string `json:"memo"`
}

type TxVSCWithdraw struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amt"`
	Token  string `json:"tk"`
	Memo   string `json:"memo"`
}

type TransactionSig struct {
	Type string `json:"__t"`
	Sigs []struct {
		Algo string `json:"alg"`
		Sig  string `json:"sig"`
		//Only applies to KeyID
		//Technically redundant as it's stored in Required_Auths
		Kid string `json:"kid"`
	} `json:"sigs"`
}

type TransactionHeader struct {
	Nonce         int64    `json:"nonce"`
	RequiredAuths []string `json:"required_auths" jsonschema:"required"`
}

// 0: Object
// 		id: "bafyreiclsfy6wld6otvy5djlvd7cu6ewyxutw26lmppzgka5odofy32liu"
// 		type: 2
// 		contract_id: "vs41q9c3ygynfp6kl86qnlaswuwvam748s5lvugns5schg4hte5vhusnx7sg5u8falrt"
// 	1: Object
// 		id: "bafyreifteiviq2ioxbnjbo6vszjjiaqxob3wdra3ve5x7qjbsmz3hnoxga"
// 		data: "iIzpEWkns0Ov47wnPg0KYYTCnB1YvKCajIXdePEDWoI"
// 		type: 5
// 		chain: "hive"

type TransactionContainer struct {
	da *datalayer.DataLayer

	//Guaranteed fields
	Id      string `json:"string"`
	TypeInt int    `json:"type"`

	Obj      map[string]interface{}
	RawBytes []byte
}

func (tx *TransactionContainer) Type() string {
	if tx.TypeInt == 1 {
		return "transaction"
	} else if tx.TypeInt == 2 {
		return "output"
	} else if tx.TypeInt == 5 {
		return "anchor"
	} else if tx.TypeInt == 6 {
		return "events"
	} else {
		return "unknown"
	}
}

// Converts to Contract Output
func (tx *TransactionContainer) AsContractOutput() *ContractOutput {
	output := ContractOutput{
		Id: tx.Id,
	}
	txCid := cid.MustParse(tx.Id)
	dag, _ := tx.da.GetDag(txCid)

	bJson, _ := dag.MarshalJSON()

	fmt.Println("Marshelled JSON from contract output", bJson)
	json.Unmarshal(bJson, &output)

	return &output
}

// As a regular VSC transaction
func (tx *TransactionContainer) AsTransaction() *OffchainTransaction {
	txCid := cid.MustParse(tx.Id)
	dag, _ := tx.da.GetDag(txCid)

	bJson, _ := dag.MarshalJSON()
	// obj := make(map[string]interface{}, 0)

	// headers := obj["headers"].(map[string]interface{})
	// Type:    obj["__t"].(string),
	// Version: obj["__v"].(string),

	// Headers: TransactionHeader{
	// 	Nonce:         headers["nonce"].(int64),
	// 	RequiredAuths: headers["required_auths"].([]string),
	// },

	// Tx: obj["tx"].(map[string]interface{}),

	fmt.Println("bJson", string(bJson))
	offchainTx := OffchainTransaction{
		Type: "hello",
	}
	json.Unmarshal(bJson, &offchainTx)
	return &offchainTx
}

// Hive anchor containing merkle root, list of hive txs
// Consider deprecating from protocol
func (tx *TransactionContainer) AsHiveAnchor() {

}

func (tx *TransactionContainer) AsEvents() {

}

func (tx *TransactionContainer) Decode(bytes []byte) {

}

type OffchainTransaction struct {
	Type    string `json:"__t" jsonschema:"required"`
	Version string `json:"__v" jsonschema:"required"`

	Headers TransactionHeader `json:"headers"`

	//This this can be any kind of object.
	Tx map[string]interface{} `json:"tx"`
}

// Verify signature of vsc transaction
// Note: VSC uses a segratated witness format (segwit) for transaction signatures
// This eliminates malleability issues and grants flexibility
// Thus signatures are separately stored and serialized
// Segwit only applies to transactions generally speaking

// Note: Signatures are verified on a 1:1 pubKey:sig structure
// In other words, signatures must be sorted the same as requiredAuths sort.
// Only applicable for multisig TXs
func (tx *OffchainTransaction) Verify(txSig TransactionSig, nonce int) (bool, error) {
	//Do verification logic using Key DID and Ethereum DID

	for idx, v := range tx.Headers.RequiredAuths {
		split := strings.Split(v, "?")
		// keyAuths = append(keyAuths, dids.KeyDID(v))
		did := dids.KeyDID(split[0])
		sig := txSig.Sigs[idx]
		verified, err := did.Verify(tx.Cid(), sig.Sig)
		if err != nil {
			return false, err
		}
		if !verified {
			return false, nil
		}
	}

	return true, nil
}

func (tx *OffchainTransaction) Encode() (*[]byte, error) {
	jsonBytes, err := json.Marshal(tx)

	if err != nil {
		return nil, err
	}

	r := bytes.NewReader(jsonBytes)
	dagNode, err := dagCbor.FromJSON(r, mh.SHA2_256, -1)

	if err != nil {
		return nil, err
	}

	// node, err := dagCbor.WrapObject(tx, mh.SHA2_256, -1)
	// fmt.Println(err)
	bytes := dagNode.RawData()
	return &bytes, nil
}

func (tx *OffchainTransaction) Decode(rawData []byte) error {
	dagNode, _ := dagCbor.Decode(rawData, mh.SHA2_256, -1)

	bytes, err := dagNode.MarshalJSON()
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, tx)
}

func (tx *OffchainTransaction) ToBlock() (*blocks.BasicBlock, error) {
	jsonBytes, err := json.Marshal(tx)

	if err != nil {
		return nil, err
	}

	r := bytes.NewReader(jsonBytes)
	block, _ := dagCbor.FromJSON(r, mh.SHA2_256, -1)

	blk, err := blocks.NewBlockWithCid(block.RawData(), block.Cid())
	return blk, err
}

func (tx *OffchainTransaction) Cid() cid.Cid {
	block, _ := tx.ToBlock()
	return block.Cid()
}

func (tx *OffchainTransaction) Ingest(se *StateEngine, txSelf TxSelf) {
	se.txDb.Ingest(transactions.IngestTransactionUpdate{
		AnchoredHeight: int64(txSelf.BlockHeight),
		AnchoredBlock:  txSelf.BlockId,
		AnchoredId:     txSelf.BlockId,
		AnchoredIndex:  int64(txSelf.Index),
		AnchoredOpIdx:  int64(txSelf.OpIndex),
		Id:             tx.Cid().String(),
		Nonce:          tx.Headers.Nonce,
		RequiredAuths:  tx.Headers.RequiredAuths,
		Tx:             tx.Tx,
	})
}

// Note: this is functionality different than original implementation
// It doesn't matter as this is just for DB serialization
// null vs bool value
func HashAuths(auths []string) *cid.Cid {
	obj := make(map[string]bool)

	for _, v := range auths {
		obj[v] = true
	}

	dag, _ := dagCbor.WrapObject(obj, mh.SHA2_256, -1)

	cid := dag.Cid()

	return &cid
}

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

var _ VSCTransaction = TxElectionResult{}
var _ VSCTransaction = TxVscHive{}
var _ VSCTransaction = TxProposeBlock{}
var _ VSCTransaction = TxCreateContract{}

type VSCTransaction interface {
	ExecuteTx(se *StateEngine)
}

// More information about the TX
type TxSelf struct {
	TxId          string
	BlockId       string
	BlockHeight   uint64
	Index         int
	OpIndex       int
	Timestamp     string
	RequiredAuths []string
}
