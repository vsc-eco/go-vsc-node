package transactionpool

import (
	"crypto/ed25519"
	"encoding/json"
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

// {
// "__t":"vsc-sig",
// "sigs":[
//
//		{
//			"alg":"EdDSA",
//			"kid":"did:key:z6MkmzUVuC9rdXtDgrfUDRJqBZKUAwpAy3k1dDscsmvK5ftb",
//			"sig":"dEQ062klY_JekaW5iX6FJss0VQLCqU9PvMAL_4Q8jJo-NwZvoM8nyJlJoa9jw9HwP6e76ChKEch-Ta-VFCF5Cw"
//		}
//		]
//	}
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
	ib.TxPool.IngestTx(tx, IngestOptions{Broadcast: true})
	return "", nil
}

type VSCTransfer struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	//NOTE: NetId should be included in headers
	NetId string `json:"-"`
	Nonce uint64 `json:"-"`
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
