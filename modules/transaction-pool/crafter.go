package transactionpool

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
	"github.com/vsc-eco/hivego"
)

type TransactionCrafter struct {
	VSCBroadcast

	Identity dids.KeyProvider
	Did      dids.DID[ed25519.PublicKey, cid.Cid]
}

func (tp *TransactionCrafter) Sign(vsOp VSCOperation) (string, error) {
	cid, _ := vsOp.Hash()
	return tp.Identity.Sign(cid)
}

// TODO: Provide option to supply already existing signature!
func (tp *TransactionCrafter) SignFinal(vscOp VSCOperation) (SerializedVSCTransaction, error) {
	sig, err := tp.Sign(vscOp)

	if err != nil {
		return SerializedVSCTransaction{}, err
	}

	sigPackage := SignaturePackage{
		Type: "vsc-sig",
		Sigs: []struct {
			Alg string `json:"alg"`
			Kid string `json:"kid"`
			Sig string `json:"sig"`
		}{
			{
				Alg: "EdDSA",
				Kid: tp.Did.String(),
				Sig: sig,
			},
		},
	}

	sTx, err := vscOp.SerializeVSC()

	if err != nil {
		return SerializedVSCTransaction{}, err
	}

	sigBytes, err := common.EncodeDagCbor(sigPackage)

	if err != nil {
		return SerializedVSCTransaction{}, err
	}

	return SerializedVSCTransaction{
		Tx:  sTx.Tx,
		Sig: sigBytes,
	}, nil
}

type SignaturePackage struct {
	Type string `json:"__t"`
	Sigs []struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Sig string `json:"sig"`
	} `json:"sigs"`
}

type VSCOperation interface {
	SerializeVSC() (SerializedVSCTransaction, error)
	SerializeHive() ([]hivego.HiveOperation, error)
	Hash() (cid.Cid, error)
}

type SerializedVSCTransaction struct {
	Tx  []byte
	Sig []byte
}

type VSCBroadcast interface {
	Broadcast(tx SerializedVSCTransaction) (string, error)
}

type InternalBroadcast struct {
	TxPool *TransactionPool
}

func (ib *InternalBroadcast) Broadcast(tx SerializedVSCTransaction) (string, error) {
	cidz, err := ib.TxPool.IngestTx(tx, IngestOptions{Broadcast: true})
	fmt.Println("err", err)
	return cidz.String(), err
}

// id = "vsc.transfer"
type VSCTransfer struct {
	From   string `json:"from"`   //From account (e.g. "did:key:z6MkmzUVuC9rdXtDgrfUDRJqBZKUAwpAy3k1dDscsmvK5ftb")
	To     string `json:"to"`     //To account (e.g. "hive:vaultec")
	Amount string `json:"amount"` //Amount in decimal format (e.g. "0.001")
	Asset  string `json:"asset"`  //Example: "hbd" or "hive" must be lowercase
	//NOTE: NetId should be included in headers
	NetId string `json:"-"` //NetId is included in custom_json as "net_id" if hive transaction; otherwise it's in headers
	Nonce uint64 `json:"-"` //Used for offchain transactions only
}

func (vt *VSCTransfer) SerializeVSC() (SerializedVSCTransaction, error) {
	recode := map[string]interface{}{}
	jjsonBytes, err := json.Marshal(vt)

	if err != nil {
		return SerializedVSCTransaction{}, err
	}

	err = json.Unmarshal(jjsonBytes, &recode)

	encodedBytes, _ := common.EncodeDagCbor(recode)

	txShell := VSCTransactionShell{
		Type:    "vsc-tx",
		Version: "0.1",
		Headers: VSCTransactionHeader{
			Nonce:         vt.Nonce,
			RequiredAuths: []string{vt.From},
			Intents:       []string{},
			NetId:         vt.NetId,
		},
		Tx: VSCTransactionData{
			Type:    "transfer",
			Payload: encodedBytes,
		},
	}

	txShellBytes, _ := common.EncodeDagCbor(txShell)

	return SerializedVSCTransaction{
		Tx: txShellBytes,
	}, nil
}

func (vt *VSCTransfer) SerializeHive() ([]hivego.HiveOperation, error) {
	serializedJson := map[string]interface{}{
		"from":   vt.From,
		"to":     vt.To,
		"amount": vt.Amount,
		"asset":  vt.Asset,
		"net_id": vt.NetId,
	}

	jsonBytes, _ := json.Marshal(serializedJson)

	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{vt.From},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.transfer",
		Json:                 string(jsonBytes),
	}

	return []hivego.HiveOperation{op}, nil
}

func (vt *VSCTransfer) Hash() (cid.Cid, error) {
	return hashVSCOperation(vt)
}

type VSCStake struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	Type   string `json:"type"`

	NetId string `json:"-"`
	Nonce uint64 `json:"-"`
}

func (tx *VSCStake) SerializeVSC() (SerializedVSCTransaction, error) {
	recode := map[string]interface{}{}
	jjsonBytes, err := json.Marshal(tx)

	if err != nil {
		return SerializedVSCTransaction{}, err
	}

	fmt.Println("jsonBytes", string(jjsonBytes))
	err = json.Unmarshal(jjsonBytes, &recode)

	if err != nil {
		return SerializedVSCTransaction{}, err
	}

	valid, err := tx.Validate()
	if !valid {
		return SerializedVSCTransaction{}, err
	}

	encodedBytes, _ := common.EncodeDagCbor(recode)

	fmt.Println("encodedBytes", string(encodedBytes))

	txShell := VSCTransactionShell{
		Type:    "vsc-tx",
		Version: "0.1",
		Headers: VSCTransactionHeader{
			Nonce:         tx.Nonce,
			RequiredAuths: []string{tx.From},
			Intents:       []string{},
			NetId:         tx.NetId,
		},
		Tx: VSCTransactionData{
			Type:    "stake",
			Payload: encodedBytes,
		},
	}

	txShellBytes, _ := common.EncodeDagCbor(txShell)

	return SerializedVSCTransaction{
		Tx: txShellBytes,
	}, nil
}

func (tx *VSCStake) SerializeHive() ([]hivego.HiveOperation, error) {
	serializedJson := map[string]interface{}{
		"from":   tx.From,
		"to":     tx.To,
		"amount": tx.Amount,
		"asset":  tx.Asset,
		"net_id": tx.NetId,
	}

	jsonBytes, _ := json.Marshal(serializedJson)

	valid, err := tx.Validate()
	if !valid {
		return nil, err
	}

	var id string
	if tx.Type == "stake" {
		id = "vsc.stake_hbd"
	} else if tx.Type == "unstake" {
		id = "vsc.unstake_hbd"
	}

	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{tx.From},
		RequiredPostingAuths: []string{},
		Id:                   id,
		Json:                 string(jsonBytes),
	}

	return []hivego.HiveOperation{op}, nil
}

func (tx *VSCStake) Hash() (cid.Cid, error) {
	return hashVSCOperation(tx)
}

func (tx *VSCStake) Validate() (bool, error) {
	fl, err := strconv.ParseFloat(tx.Amount, 64)
	if err != nil {
		return false, err
	}
	if fl < 0.001 {
		return false, fmt.Errorf("failed validation - amount must be greater or equal to 0.001")
	}
	if tx.NetId == "" {
		return false, fmt.Errorf("failed validation - net_id must be set")
	}
	if tx.To == "" || tx.From == "" {
		return false, fmt.Errorf("failed validation - to and from must be set")
	}
	valid := tx.Asset == "hbd" || tx.Asset == "hbd_savings" && (tx.Type == "stake" || tx.Type == "unstake")

	if !valid {
		return false, fmt.Errorf("failed validation - invalid asset or type; asset must equal hbd, and stake = stake|unstake")
	}

	return valid, nil
}

type VscConsenusStake struct {
	Account string `json:"account"`
	Amount  string `json:"amount"`
	Type    string `json:"type"`

	NetId string `json:"-"`
	Nonce uint64 `json:"-"`
}

func (tx *VscConsenusStake) SerializeVSC() (SerializedVSCTransaction, error) {
	return SerializedVSCTransaction{}, errors.New("Unsupported operation")
}

func (tx *VscConsenusStake) SerializeHive() ([]hivego.HiveOperation, error) {
	serializedJson := map[string]interface{}{
		"account": tx.Account,
		"amount":  tx.Amount,
		"net_id":  tx.NetId,
	}

	jsonBytes, _ := json.Marshal(serializedJson)

	valid, err := tx.Validate()
	if !valid {
		return nil, err
	}

	var id string
	if tx.Type == "stake" {
		id = "vsc.consensus_stake"
	} else if tx.Type == "unstake" {
		id = "vsc.consensus_unstake"
	}

	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{tx.Account},
		RequiredPostingAuths: []string{},
		Id:                   id,
		Json:                 string(jsonBytes),
	}

	return []hivego.HiveOperation{op}, nil
}

func (tx *VscConsenusStake) Hash() (cid.Cid, error) {
	return hashVSCOperation(tx)
}

func (tx *VscConsenusStake) Validate() (bool, error) {
	if tx.NetId == "" {
		return false, fmt.Errorf("failed validation - net_id must be set")
	}
	if tx.Account == "" {
		return false, fmt.Errorf("failed validation - account must be set")
	}
	if tx.Type != "stake" && tx.Type != "unstake" {
		return false, fmt.Errorf("failed validation - type must be stake or unstake")
	}
	if _, err := strconv.ParseFloat(tx.Amount, 64); err != nil {
		return false, fmt.Errorf("failed validation - amount must be a valid number")
	}

	return true, nil
}

type VscWithdraw struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	Type   string `json:"type"`

	NetId string `json:"-"`
	Nonce uint64 `json:"-"`
}

func (tx *VscWithdraw) SerializeVSC() (SerializedVSCTransaction, error) {
	recode := map[string]interface{}{}
	jjsonBytes, err := json.Marshal(tx)

	if err != nil {
		return SerializedVSCTransaction{}, err
	}

	err = json.Unmarshal(jjsonBytes, &recode)

	if err != nil {
		return SerializedVSCTransaction{}, err
	}

	valid, err := tx.Validate()
	if !valid {
		return SerializedVSCTransaction{}, err
	}

	encodedBytes, _ := common.EncodeDagCbor(recode)

	txShell := VSCTransactionShell{
		Type:    "vsc-tx",
		Version: "0.1",
		Headers: VSCTransactionHeader{
			Nonce:         tx.Nonce,
			RequiredAuths: []string{tx.From},
			Intents:       []string{},
			NetId:         tx.NetId,
		},
		Tx: VSCTransactionData{
			Type:    "withdraw",
			Payload: encodedBytes,
		},
	}

	txShellBytes, _ := common.EncodeDagCbor(txShell)

	return SerializedVSCTransaction{
		Tx: txShellBytes,
	}, nil
}

func (tx *VscWithdraw) SerializeHive() ([]hivego.HiveOperation, error) {
	serializedJson := map[string]interface{}{
		"from":   tx.From,
		"to":     tx.To,
		"amount": tx.Amount,
		"asset":  tx.Asset,
		"net_id": tx.NetId,
	}

	jsonBytes, _ := json.Marshal(serializedJson)

	valid, err := tx.Validate()
	if !valid {
		return nil, err
	}

	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{strings.Split(tx.From, ":")[1]},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.withdraw",
		Json:                 string(jsonBytes),
	}

	return []hivego.HiveOperation{op}, nil
}

func (tx *VscWithdraw) Hash() (cid.Cid, error) {
	return hashVSCOperation(tx)
}

func (tx *VscWithdraw) Validate() (bool, error) {
	fl, err := strconv.ParseFloat(tx.Amount, 64)
	if err != nil {
		return false, err
	}
	if fl < 0.001 {
		return false, fmt.Errorf("failed validation - amount must be greater or equal to 0.001")
	}
	if tx.NetId == "" {
		return false, fmt.Errorf("failed validation - net_id must be set")
	}
	if tx.To == "" || tx.From == "" {
		return false, fmt.Errorf("failed validation - to and from must be set")
	}
	valid := tx.Asset == "hbd" || tx.Asset == "hive"

	if !valid {
		return false, fmt.Errorf("failed validation - invalid asset or type; asset must equal hbd, and stake = stake|unstake")
	}

	return valid, nil
}

func hashVSCOperation(tx VSCOperation) (cid.Cid, error) {
	serialized, err := tx.SerializeVSC()

	if err != nil {
		return cid.Cid{}, err
	}

	cidPrefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   multihash.SHA2_256,
		MhLength: -1,
	}
	return cidPrefix.Sum(serialized.Tx)
}

// {
//     "__t": "vsc-tx",
//     "__v": "0.2",
//     "headers": {
//         "nonce": 1043,
//         "required_auths": [
//             "did:key:z6MkmzUVuC9rdXtDgrfUDRJqBZKUAwpAy3k1dDscsmvK5ftb"
//         ]
//     },
//     "tx": {
//         "op": "call_contract",
//         "payload": {},
//         "type": 1
//     }
// }

type VSCTransactionShell struct {
	Type    string               `json:"__t"`
	Version string               `json:"__v"`
	Headers VSCTransactionHeader `json:"headers"`
	Tx      VSCTransactionData   `json:"tx"`
}

type VSCTransactionHeader struct {
	Nonce         uint64   `json:"nonce"`
	RequiredAuths []string `json:"required_auths"`
	Intents       []string `json:"intents"`
	NetId         string   `json:"net_id"`
}

type VSCTransactionData struct {
	Type    string `json:"type"`
	Payload []byte `json:"payload"`
}
