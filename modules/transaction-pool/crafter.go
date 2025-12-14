package transactionpool

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/contracts"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	dagCbor "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multicodec"
	multihash "github.com/multiformats/go-multihash/core"

	"github.com/vsc-eco/hivego"
)

type TransactionCrafter struct {
	VSCBroadcast

	Identity dids.Provider[blocks.Block]
	Did      dids.DID
}

func (tp *TransactionCrafter) Sign(vsOp VSCTransaction) (string, error) {
	// _, signableHash, _ := vsOp.Hash()

	// cid, _, err := vsOp.Hash()

	// fmt.Println("sign hash bytes", cid.Bytes(), err)

	// fmt.Println("Preparing to sign VSC operation", vsOp)
	blk, err := vsOp.ToSignableBlock()

	// fmt.Println("signable err", err)
	if err != nil {
		return "", fmt.Errorf("could not create signable block: %w", err)
	}

	// fmt.Println("signable block", blk)
	signedRet, err := tp.Identity.Sign(blk)

	// fmt.Println("signable block signed", signedRet, err)
	return signedRet, err
	// return "", errors.New("Signing not implemented yet")
}

// TODO: Provide option to supply already existing signature!
func (tp *TransactionCrafter) SignFinal(vscTx VSCTransaction) (SerializedVSCTransaction, error) {
	sig, err := tp.Sign(vscTx)

	if err != nil {
		return SerializedVSCTransaction{}, err
	}

	fmt.Println("common.Sig", sig)

	sigPackage := SignaturePackage{
		Type: "vsc-sig",
		Sigs: []common.Sig{
			{
				Algo: "EdDSA",
				Kid:  tp.Did.String(),
				Sig:  sig,
			},
		},
	}

	sTx, err := vscTx.Serialize()

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
	Type string       `refmt:"__t" json:"__t"`
	Sigs []common.Sig `refmt:"sigs" json:"sigs"`
}

type VSCOperation interface {
	SerializeVSC() (VSCTransactionOp, error)
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
}

func (vt *VSCTransfer) SerializeVSC() (VSCTransactionOp, error) {
	recode := map[string]interface{}{}
	jjsonBytes, err := json.Marshal(vt)

	if err != nil {
		return VSCTransactionOp{}, err
	}

	err = json.Unmarshal(jjsonBytes, &recode)

	encodedBytes, _ := common.EncodeDagCbor(recode)

	return VSCTransactionOp{
		Type:    "transfer",
		Payload: encodedBytes,

		RequiredAuths: struct {
			Active  []string
			Posting []string
		}{
			Active: []string{vt.From},
		},
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

	if !strings.HasPrefix(vt.From, "hive:") {
		return nil, fmt.Errorf("cannot serialize if from is not hive account")
	}

	jsonBytes, _ := json.Marshal(serializedJson)

	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{strings.Split(vt.From, ":")[1]},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.transfer",
		Json:                 string(jsonBytes),
	}

	return []hivego.HiveOperation{op}, nil
}

// func (vt *VSCTransfer) Hash() (cid.Cid, error) {
// 	return hashVSCOperation(vt)
// }

type VSCStake struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	Type   string `json:"type"`

	NetId string `json:"-"`
}

func (tx *VSCStake) SerializeVSC() (VSCTransactionOp, error) {
	recode := map[string]interface{}{}
	jjsonBytes, err := json.Marshal(tx)

	if err != nil {
		return VSCTransactionOp{}, err
	}

	err = json.Unmarshal(jjsonBytes, &recode)

	if err != nil {
		return VSCTransactionOp{}, err
	}

	valid, err := tx.Validate()
	if !valid {
		return VSCTransactionOp{}, err
	}

	encodedBytes, _ := common.EncodeDagCbor(recode)

	return VSCTransactionOp{
		Type:    tx.Type,
		Payload: encodedBytes,
		RequiredAuths: struct {
			Active  []string
			Posting []string
		}{
			Active: []string{tx.From, tx.To},
		},
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

	if !strings.HasPrefix(tx.From, "hive:") {
		return nil, fmt.Errorf("cannot serialize if from is not hive account")
	}

	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{strings.Split(tx.From, ":")[1]},
		RequiredPostingAuths: []string{},
		Id:                   id,
		Json:                 string(jsonBytes),
	}

	return []hivego.HiveOperation{op}, nil
}

// func (tx *VSCStake) Hash() (cid.Cid, error) {
// 	return hashVSCOperation(tx)
// }

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

	if !strings.HasPrefix(tx.Account, "hive:") {
		return nil, fmt.Errorf("cannot serialize if from is not hive account")
	}

	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{strings.Split(tx.Account, ":")[1]},
		RequiredPostingAuths: []string{},
		Id:                   id,
		Json:                 string(jsonBytes),
	}

	return []hivego.HiveOperation{op}, nil
}

// func (tx *VscConsenusStake) Hash() (cid.Cid, error) {
// 	return hashVSCOperation(tx)
// }

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
}

func (tx *VscWithdraw) SerializeVSC() (VSCTransactionOp, error) {
	recode := map[string]interface{}{}
	jjsonBytes, err := json.Marshal(tx)

	if err != nil {
		return VSCTransactionOp{}, err
	}

	err = json.Unmarshal(jjsonBytes, &recode)

	if err != nil {
		return VSCTransactionOp{}, err
	}

	valid, err := tx.Validate()
	if !valid {
		return VSCTransactionOp{}, err
	}

	encodedBytes, _ := common.EncodeDagCbor(recode)

	return VSCTransactionOp{
		Type:    "withdraw",
		Payload: encodedBytes,

		RequiredAuths: struct {
			Active  []string
			Posting []string
		}{
			Active: []string{tx.From},
		},
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

	if !strings.HasPrefix(tx.From, "hive:") {
		return nil, fmt.Errorf("cannot serialize if from is not hive account")
	}

	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{strings.Split(tx.From, ":")[1]},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.withdraw",
		Json:                 string(jsonBytes),
	}

	return []hivego.HiveOperation{op}, nil
}

// func (tx *VscWithdraw) Hash() (cid.Cid, error) {
// 	return hashVSCOperation(tx)
// }

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

type VscContractCall struct {
	ContractId string             `json:"contract_id"`
	Action     string             `json:"action"`
	Payload    string             `json:"payload"`
	RcLimit    uint               `json:"rc_limit"`
	Intents    []contracts.Intent `json:"intents"`

	Caller string `json:"caller"`

	NetId string `json:"-"`
}

func (tx *VscContractCall) SerializeVSC() (VSCTransactionOp, error) {
	recode := map[string]interface{}{}
	jjsonBytes, err := json.Marshal(tx)

	if err != nil {
		return VSCTransactionOp{}, err
	}

	err = json.Unmarshal(jjsonBytes, &recode)

	if err != nil {
		return VSCTransactionOp{}, err
	}

	valid, err := tx.Validate()
	if !valid {
		return VSCTransactionOp{}, err
	}

	encodedBytes, _ := common.EncodeDagCbor(recode)

	return VSCTransactionOp{
		Type:    "call",
		Payload: encodedBytes,

		RequiredAuths: struct {
			Active  []string
			Posting []string
		}{
			Active: []string{tx.Caller},
		},
	}, nil
}

func (tx *VscContractCall) SerializeHive() ([]hivego.HiveOperation, error) {
	serializedJson := map[string]interface{}{
		"contract_id": tx.ContractId,
		"action":      tx.Action,
		"payload":     tx.Payload,
		"rc_limit":    tx.RcLimit,
		"intents":     tx.Intents,
		"caller":      tx.Caller,
		"net_id":      tx.NetId,
	}

	jsonBytes, _ := json.Marshal(serializedJson)

	valid, err := tx.Validate()
	if !valid {
		return nil, err
	}

	postingMap := make(map[string]bool, 0)
	activeMap := make(map[string]bool, 0)

	for _, intent := range tx.Intents {
		fmt.Println("intent", intent)
		if intent.Type == "transfer.allow" {
			activeMap[tx.Caller] = true
		}
	}

	if !activeMap[tx.Caller] {
		postingMap[tx.Caller] = true
	}

	posting := make([]string, 0)
	active := make([]string, 0)
	for k := range postingMap {
		posting = append(posting, strings.Split(k, ":")[1])
	}
	for k := range activeMap {
		active = append(active, strings.Split(k, ":")[1])
	}

	op := hivego.CustomJsonOperation{
		RequiredAuths:        active,
		RequiredPostingAuths: posting,
		Id:                   "vsc.call",
		Json:                 string(jsonBytes),
	}

	return []hivego.HiveOperation{op}, nil
}

func (tx *VscContractCall) Validate() (bool, error) {
	if !strings.HasPrefix(tx.Caller, "did:") && !strings.HasPrefix(tx.Caller, "hive:") {
		return false, fmt.Errorf("failed validation - caller must be set")
	}
	if !strings.HasPrefix(tx.ContractId, "vsc1") {
		return false, fmt.Errorf("failed validation - contract_id must be set")
	}
	if tx.Action == "" {
		return false, fmt.Errorf("failed validation - action must be set")
	}
	if tx.Payload == "" {
		return false, fmt.Errorf("failed validation - payload must be set")
	}
	return true, nil
}

// func hashVSCOperation(tx VSCOperation) (cid.Cid, error) {
// 	serialized, err := tx.SerializeVSC()

// 	if err != nil {
// 		return cid.Cid{}, err
// 	}

// 	cidPrefix := cid.Prefix{
// 		Version:  1,
// 		Codec:    uint64(multicodec.DagCbor),
// 		MhType:   multihash.SHA2_256,
// 		MhLength: -1,
// 	}

// 	return cidPrefix.Sum(serialized.Tx)
// }

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

type VSCTransaction struct {
	Ops   []VSCTransactionOp
	Nonce uint64
	NetId string // NetId is included in headers
}

func (tx *VSCTransaction) Serialize() (SerializedVSCTransaction, error) {
	txShell := tx.ToShell()

	serialized, err := common.EncodeDagCbor(txShell)

	return SerializedVSCTransaction{
		Tx: serialized,
	}, err
}

func (tx *VSCTransaction) ToShell() VSCTransactionShell {
	requiredAuthsMap := map[string]bool{}

	for _, op := range tx.Ops {
		for _, auth := range op.RequiredAuths.Active {
			requiredAuthsMap[auth] = true
		}
	}

	requiredAuths := []string{}
	for auth := range requiredAuthsMap {
		requiredAuths = append(requiredAuths, auth)
	}

	shell := VSCTransactionShell{
		Type:    "vsc-tx",
		Version: "0.2",
		Headers: VSCTransactionHeader{
			Nonce:         tx.Nonce,
			RequiredAuths: requiredAuths,
			NetId:         tx.NetId,
			RcLimit:       500,
		},
		Tx: tx.Ops,
	}

	return shell
}

func (tx *VSCTransaction) ToSignableBlock() (blocks.Block, error) {
	shell := tx.ToShell()

	ops := make([]VSCTransactionSignOp, 0)
	for _, op := range shell.Tx {
		var serialized string
		dagNode, err := dagCbor.Decode(op.Payload, multihash.SHA2_256, -1)

		if err != nil {
			return &blocks.BasicBlock{}, fmt.Errorf("failed to decode payload: %w", err)
		}

		jjbytes, err := dagNode.MarshalJSON()

		if err != nil {
			return &blocks.BasicBlock{}, fmt.Errorf("failed to marshal payload to JSON: %w", err)
		}
		serialized = string(jjbytes)

		ops = append(ops, VSCTransactionSignOp{
			Type:    op.Type,
			Payload: serialized,
		})
	}

	signingShell2 := map[string]interface{}{
		"__t": shell.Type,
		"__v": shell.Version,
		"headers": VSCTransactionHeader{
			Nonce:         shell.Headers.Nonce,
			RequiredAuths: shell.Headers.RequiredAuths,
			NetId:         shell.Headers.NetId,
			RcLimit:       shell.Headers.RcLimit,
		},
		"tx": ops,
	}

	// ssbytes, _ := json.Marshal(signingShell2)
	// fmt.Println("signingShell2", string(ssbytes))

	bytes, _ := common.EncodeDagCbor(signingShell2)

	cidPrefix, _ := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   multihash.SHA2_256,
		MhLength: -1,
	}.Sum(bytes)

	return blocks.NewBlockWithCid(bytes, cidPrefix)
}

func (tx *VSCTransaction) HashEip712() (cid.Cid, []byte, error) {
	shell := tx.ToShell()

	ops := make([]VSCTransactionSignOp, 0)
	for _, op := range shell.Tx {
		var serialized string
		dagNode, err := dagCbor.Decode(op.Payload, multihash.SHA2_256, -1)

		if err != nil {
			return cid.Cid{}, nil, fmt.Errorf("failed to decode payload: %w", err)
		}

		jjbytes, err := dagNode.MarshalJSON()

		if err != nil {
			return cid.Cid{}, nil, fmt.Errorf("failed to marshal payload to JSON: %w", err)
		}
		serialized = string(jjbytes)

		ops = append(ops, VSCTransactionSignOp{
			Type:    op.Type,
			Payload: serialized,
		})
	}

	signingShell2 := map[string]interface{}{
		"__t": shell.Type,
		"__v": shell.Version,
		"headers": VSCTransactionHeader{
			Nonce:         shell.Headers.Nonce,
			RequiredAuths: shell.Headers.RequiredAuths,
			NetId:         shell.Headers.NetId,
			RcLimit:       shell.Headers.RcLimit,
		},
		"tx": ops,
	}

	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", signingShell2, "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})

	if err != nil {
		return cid.Cid{}, nil, fmt.Errorf("failed to convert to EIP712 typed data: %w", err)
	}

	bytes, err := typedData.Data.HashStruct("tx_container_v0", signingShell2)

	if err != nil {
		return cid.Cid{}, nil, fmt.Errorf("failed to hash struct: %w", err)
	}

	byteArray := append([]byte{multihash.KECCAK_256}, bytes...) // Add multihash prefix byte
	return cid.NewCidV1(uint64(multicodec.DagCbor), byteArray), bytes, nil
}

func (tx *VSCTransaction) Hash() (cid.Cid, []byte, error) {
	shell := tx.ToShell()

	ops := make([]VSCTransactionSignOp, 0)
	for _, op := range shell.Tx {
		var serialized string
		dagNode, err := dagCbor.Decode(op.Payload, multihash.SHA2_256, -1)

		if err != nil {
			return cid.Cid{}, nil, fmt.Errorf("failed to decode payload: %w", err)
		}

		jjbytes, err := dagNode.MarshalJSON()

		if err != nil {
			return cid.Cid{}, nil, fmt.Errorf("failed to marshal payload to JSON: %w", err)
		}
		serialized = string(jjbytes)

		ops = append(ops, VSCTransactionSignOp{
			Type:    op.Type,
			Payload: serialized,
		})
	}

	// signingShell := VSCTransactionSignStruct{
	// 	Type:    shell.Type,
	// 	Version: shell.Version,
	// 	Headers: VSCTransactionHeader{
	// 		Nonce:         shell.Headers.Nonce,
	// 		RequiredAuths: shell.Headers.RequiredAuths,
	// 		NetId:         shell.Headers.NetId,
	// 	},
	// 	Tx: ops,
	// }

	signingShell2 := map[string]interface{}{
		"__t": shell.Type,
		"__v": shell.Version,
		"headers": VSCTransactionHeader{
			Nonce:         shell.Headers.Nonce,
			RequiredAuths: shell.Headers.RequiredAuths,
			NetId:         shell.Headers.NetId,
			RcLimit:       shell.Headers.RcLimit,
		},
		"tx": ops,
	}

	ssbytes, _ := json.Marshal(signingShell2)
	fmt.Println("signingShell2", string(ssbytes))

	bytes, _ := common.EncodeDagCbor(signingShell2)

	cidPrefix, _ := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   multihash.SHA2_256,
		MhLength: -1,
	}.Sum(bytes)

	return cidPrefix, cidPrefix.Bytes(), nil
}
