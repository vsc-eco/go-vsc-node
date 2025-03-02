package stateEngine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"

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

type TxProposeBlock struct {
	Self TxSelf

	//ReplayId should be deprecated soon
	NetId       string            `json:"net_id"`
	SignedBlock SignedBlockHeader `json:"signed_block"`
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
func (t TxProposeBlock) ExecuteTx(se *StateEngine) {
	blockCid, _ := cid.Parse(t.SignedBlock.Block)
	node, _ := se.da.GetDag(blockCid)
	jsonBytes, _ := node.MarshalJSON()
	blockContentC := vscBlocks.VscBlock{}
	// json.Unmarshal(jsonBytes, &blockContent)

	err := se.da.GetObject(blockCid, &blockContentC, datalayer.GetOptions{})

	fmt.Println("399 err GetObject", err, blockContentC)

	bbyes, _ := json.Marshal(blockContentC)
	fmt.Println("Decoded VSC Block header", string(bbyes))
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
		Ts: t.Self.Timestamp,
	})

	txsToInjest := make([]VSCTransaction, 0)
	//At this point of the process a call should be made to state engine
	//To kick off finalization of the inflight state
	//Such as transfers, contract calls, etc
	//New TXs should be indexed at this point
	for idx, txInfo := range blockContentC.Transactions {
		tx := BlockTx{
			Id:   txInfo.Id,
			Op:   *txInfo.Op,
			Type: txInfo.Type,
		}
		//Things to Process
		// - Contract executionll
		// - Transfers, withdraws
		// - New TXs (repeat process in state engine)
		//Note: VSC txs can be processed immediately once anchored on chain
		//Thus: TX confirmation is 30s maximum
		//Author: @vaultec81

		txContainer := tx.Decode(se.da)
		fmt.Println("Iterating transaction check", tx, txContainer.Type())

		if txContainer.Type() == "transaction" {
			//Note: sig verification has already happened
			tx := txContainer.AsTransaction()

			fmt.Println("INGESTING TRANSACTION", tx, txContainer)

			tx.Ingest(se, TxSelf{
				BlockId:     t.Self.BlockId,
				BlockHeight: t.Self.BlockHeight,
				Index:       t.Self.Index,
				OpIndex:     idx,
			})

			txsToInjest = append(txsToInjest, tx)

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
	Id string `json:"id"`
	Op string `json:"op,omitempty"`
	// 1 input
	// 2 output
	// 5 anchor
	// 6 oplog

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

func (tx TxVscHive) TxSelf() TxSelf {
	return tx.Self
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
	Self TxSelf

	NetId string `json:"net_id"`

	From   string `json:"from"`
	To     string `json:"to"`
	Amount int64  `json:"amount"`
	Token  string `json:"token"`
	Memo   string `json:"memo"`
}

func (tx TxVSCWithdraw) TxSelf() TxSelf {
	return tx.Self
}

// Development note:
// t.From is a slightly different field from t.Self.RequiredAuths[0]
// It must exist this way for cosigned transaction support.
func (t *TxVSCWithdraw) ExecuteTx(se *StateEngine) {
	if t.NetId != common.NETWORK_ID {
		return
	}
	if t.To == "" {
		return
	}
	params := WithdrawParams{
		Id:     MakeTxId(t.Self.TxId, t.Self.OpIndex),
		BIdx:   int64(t.Self.Index),
		OpIdx:  int64(t.Self.OpIndex),
		To:     t.To,
		Asset:  t.Token,
		Memo:   t.Memo,
		Amount: t.Amount,
	}
	if t.From == "" {
		params.From = "hive:" + t.Self.RequiredAuths[0]
	} else {
		params.From = t.From
	}

	//Verifies
	if !slices.Contains(t.Self.RequiredAuths, strings.Split(t.From, ":")[1]) {
		return
	}

	se.LedgerExecutor.Withdraw(params)

	// fmt.Println("Executed ledgerResult", ledgerResult)
	// fmt.Println("se Oplog", se.LedgerExecutor.Oplog)
	// fmt.Println("se VirtualLedger", se.LedgerExecutor.VirtualLedger)
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
	Nonce         uint64   `json:"nonce"`
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

	fmt.Println("bJson", string(bJson))
	offchainTx := OffchainTransaction{
		TxId: tx.Id,
	}
	json.Unmarshal(bJson, &offchainTx)

	// b64Bytes, _ := base64.StdEncoding.DecodeString(offchainTx.Tx["payload"].(string))

	// node, _ := cbornode.Decode(b64Bytes, mh.SHA2_256, -1)
	// bbytes, _ := node.MarshalJSON()
	// var txPayload map[string]interface{}
	// json.Unmarshal(bbytes, &txPayload)

	// offchainTx.Tx = map[string]interface{}{
	// 	"type":    offchainTx.Tx["op"],
	// 	"payload": txPayload,
	// }

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
	TxId string `json:"-"`

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

func (tx *OffchainTransaction) Encode() ([]byte, error) {
	return common.EncodeDagCbor(tx)
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
	encodedBytes, _ := common.EncodeDagCbor(tx)

	prefix := cid.Prefix{
		Version:  1,
		Codec:    cid.DagCBOR,
		MhType:   mh.SHA2_256,
		MhLength: -1,
	}

	cid, err := prefix.Sum(encodedBytes)

	if err != nil {
		return nil, err
	}

	blk, err := blocks.NewBlockWithCid(encodedBytes, cid)
	return blk, err
}

func (tx *OffchainTransaction) Cid() cid.Cid {
	txId := cid.MustParse(tx.TxId)
	return txId
}

func (tx *OffchainTransaction) Ingest(se *StateEngine, txSelf TxSelf) {
	anchoredHeight := int64(txSelf.BlockHeight)
	anchoredIndex := int64(txSelf.Index)
	anchoredOpIdx := int64(txSelf.OpIndex)

	fmt.Println("Ingesting the following tx", tx.Tx)
	se.txDb.Ingest(transactions.IngestTransactionUpdate{
		Status:         "INCLUDED",
		Id:             tx.Cid().String(),
		AnchoredIndex:  &anchoredIndex,
		AnchoredOpIdx:  &anchoredOpIdx,
		AnchoredHeight: &anchoredHeight,
		AnchoredBlock:  &txSelf.BlockId,
		AnchoredId:     &txSelf.BlockId,
		Nonce:          tx.Headers.Nonce,
		RequiredAuths:  tx.Headers.RequiredAuths,
		Tx:             tx.Tx,
	})

}

func (tx *OffchainTransaction) ExecuteTx(se *StateEngine) {

}

func (tx *OffchainTransaction) TxSelf() TxSelf {
	return TxSelf{}
}

func (tx *OffchainTransaction) ToTransaction() VSCTransaction {

	return nil
}

var _ VSCTransaction = TxElectionResult{}
var _ VSCTransaction = TxVscHive{}
var _ VSCTransaction = TxProposeBlock{}
var _ VSCTransaction = TxCreateContract{}

type VSCTransaction interface {
	ExecuteTx(se *StateEngine)
	TxSelf() TxSelf
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
