package stateEngine

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/transactions"

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

type TxVscCallContract struct {
	Self  TxSelf
	NetId string `json:"net_id"`

	//Excluded RC balance for RC allocations
	//Note: Hive users will get small limit of RCs by default
	//Hold more hbd to get more RCs
	RcExclusion uint64 `json:"rc_exclusion"`

	Op         string `json:"op"`
	ContractId string `json:"contract_id"`
	Payload    string `json:"payload"`
}

func (tx TxVscCallContract) Type() string {
	return "call_contract"
}

func (tx TxVscCallContract) ExecuteTx(se *StateEngine) {
	//Hook up to the contract executor

	//Access to ledger executor, access to IPFS state
	//Access to ??

	// se.LedgerExecutor.ExecuteTransfer()

}

func (tx TxVscCallContract) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxVscCallContract) ToData() map[string]interface{} {
	return map[string]interface{}{
		"type":        "call",
		"op":          tx.Op,
		"contract_id": tx.ContractId,
		"payload":     tx.Payload,
	}
}

// Costs no RCs as it consumes Hive RCs.
// Only applies if original transaction is Hive
type TxVSCTransfer struct {
	Self  TxSelf
	NetId string `json:"net_id"`

	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	Memo   string `json:"memo"`
}

func (tx TxVSCTransfer) Type() string {
	return "transfer"
}

func (tx TxVSCTransfer) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxVSCTransfer) ExecuteTx(se *StateEngine) {
	fmt.Println("Executing transfer tx", tx)
	// if tx.NetId != common.NETWORK_ID {
	// 	return
	// }
	if tx.To == "" || tx.From == "" {
		return
	}

	if !slices.Contains(tx.Self.RequiredAuths, tx.From) {
		return
	}
	amount, _ := strconv.ParseFloat(tx.Amount, 64)

	amt := amount * math.Pow(10, 3)

	transferParams := OpLogEvent{
		Id:     MakeTxId(tx.Self.TxId, tx.Self.OpIndex),
		BIdx:   int64(tx.Self.Index),
		OpIdx:  int64(tx.Self.OpIndex),
		From:   tx.From,
		To:     tx.To,
		Amount: int64(amt),
		Asset:  tx.Asset,
		Memo:   tx.Memo,
	}

	ledgerResult := se.LedgerExecutor.ExecuteTransfer(transferParams)

	fmt.Println("LedgerResult", ledgerResult)
}

func (tx TxVSCTransfer) ToData() map[string]interface{} {
	return map[string]interface{}{
		"from":   tx.From,
		"to":     tx.To,
		"amount": tx.Amount,
		"asset":  tx.Asset,
		"memo":   tx.Memo,
	}
}

type TxVSCWithdraw struct {
	Self  TxSelf
	NetId string `json:"net_id"`

	From   string `json:"from"`
	To     string `json:"to"`
	Amount int64  `json:"amount"`
	Token  string `json:"token"`
	Memo   string `json:"memo"`
}

func (tx TxVSCWithdraw) Type() string {
	return "withdraw"
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

}

func (tx *TxVSCWithdraw) ToData() map[string]interface{} {
	return map[string]interface{}{
		"from":   tx.From,
		"to":     tx.To,
		"amount": tx.Amount,
		"token":  tx.Token,
		"memo":   tx.Memo,
	}
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
	Self TxSelf

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
		return "oplog"
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
		Self: TxSelf{
			TxId: tx.Id,
		},
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

func (tx *TransactionContainer) AsOplog() Oplog {
	cid := cid.MustParse(tx.Id)
	node, err := tx.da.GetDag(cid)

	if err != nil {
		panic(err)
	}
	jsonBytes, _ := node.MarshalJSON()
	fmt.Println("Oplog node", node, string(jsonBytes))

	oplog := Oplog{
		Self: tx.Self,
	}
	json.Unmarshal(jsonBytes, &oplog)
	fmt.Println("Oplog decoded", oplog)

	return oplog
}

func (tx *TransactionContainer) Decode(bytes []byte) {

}

type Oplog struct {
	Self TxSelf

	Oplog []OpLogEvent `json:"oplog"`
}

func (oplog *Oplog) ExecuteTx(se *StateEngine) {
	se.LedgerExecutor.Flush()

	aoplog := make([]OpLogEvent, 0)
	for _, v := range oplog.Oplog {
		v.BlockHeight = oplog.Self.BlockHeight
		aoplog = append(aoplog, v)
	}
	se.LedgerExecutor.Ls.IngestOplog(aoplog)
}

type OffchainTransaction struct {
	Self TxSelf

	TxId string `json:"-"`

	DataType string `json:"__t" jsonschema:"required"`
	Version  string `json:"__v" jsonschema:"required"`

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

	parsedTx := tx.ToTransaction()[0]

	data := make(map[string]interface{})

	for k, v := range parsedTx.ToData() {
		data[k] = v
	}

	data["type"] = parsedTx.Type()

	fmt.Println("Ingesting the following tx", tx.Tx)
	se.txDb.Ingest(transactions.IngestTransactionUpdate{
		Status:         "INCLUDED",
		Id:             tx.TxId,
		AnchoredIndex:  &anchoredIndex,
		AnchoredOpIdx:  &anchoredOpIdx,
		AnchoredHeight: &anchoredHeight,
		AnchoredBlock:  &txSelf.BlockId,
		AnchoredId:     &txSelf.BlockId,
		Nonce:          tx.Headers.Nonce,
		RequiredAuths:  tx.Headers.RequiredAuths,
		Tx:             data,
	})

}

func (tx *OffchainTransaction) TxSelf() TxSelf {
	return tx.Self
}

func (tx *OffchainTransaction) ToTransaction() []VSCTransaction {
	self := tx.TxSelf()
	self.RequiredAuths = tx.Headers.RequiredAuths

	var vtx VSCTransaction
	switch tx.Tx["type"].(string) {
	case "call":
		callTx := TxVscCallContract{
			Self: self,
		}
		vtx = callTx
	case "transfer":
		transferTx := TxVSCTransfer{
			Self: self,
		}
		decodeErr := decodeTxCbor(tx, &transferTx)

		bbytes, _ := json.Marshal(transferTx)
		fmt.Println("Decoded transfer tx", string(bbytes), decodeErr)
		vtx = transferTx
	case "withdraw":
		withdrawTx := TxVSCWithdraw{
			Self: self,
		}
		decodeTxCbor(tx, &withdrawTx)

		vtx = &withdrawTx
	case "stake_hbd":

	case "unstake_hbd":

	}

	fmt.Println("Maybe transfer tx", vtx, tx.Type())

	if vtx == nil {
		return nil
	}

	return []VSCTransaction{vtx}
}

func (tx *OffchainTransaction) Type() string {

	return "offchain"
}

var _ VSCTransaction = &TxElectionResult{}

// var _ VSCTransaction = &TxProposeBlock{}
var _ VSCTransaction = &TxCreateContract{}

var _ VscTxContainer = &OffchainTransaction{}

type VSCTransaction interface {
	ExecuteTx(se *StateEngine)
	TxSelf() TxSelf
	ToData() map[string]interface{}
	Type() string
}

type VscTxContainer interface {
	Type() string //Hive, offchain
	ToTransaction() []VSCTransaction
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
