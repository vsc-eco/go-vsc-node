package transactionpool

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/transactions"
	libp2p "vsc-node/modules/p2p"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
)

type TransactionPool struct {
	TxDb      transactions.Transactions
	p2p       *libp2p.P2PServer
	service   libp2p.PubSubService[p2pMessage]
	datalayer *datalayer.DataLayer

	conf common.IdentityConfig
}

type IngestOptions struct {
	Broadcast bool
}

// Ingests and verifies a transaction
func (tp *TransactionPool) IngestTx(sTx SerializedVSCTransaction, options ...IngestOptions) (*cid.Cid, error) {
	if sTx.Sig == nil {
		return nil, errors.New("No signature provided")
	}

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(multihash.SHA2_256),
		MhLength: -1,
	}

	cidz, err := prefix.Sum(sTx.Tx)
	if err != nil {
		return nil, err
	}

	tempObj := map[string]interface{}{}

	node, err := cbornode.Decode(sTx.Sig, multihash.SHA2_256, -1)
	if err != nil {
		return nil, err
	}

	jsonData, err := node.MarshalJSON()
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(jsonData, &tempObj)
	if err != nil {
		return nil, err
	}

	verifiedDids := make([]dids.KeyDID, 0)

	for _, v := range tempObj["sigs"].([]interface{}) {
		sig := v.(map[string]interface{})

		if sig["alg"] != "EdDSA" {
			return nil, errors.New("Unsupported signature algorithm")
		}

		if sig["kid"] == nil {
			return nil, errors.New("No key id provided")
		}

		if sig["sig"] == nil {
			return nil, errors.New("No signature provided")
		}

		keyDid := dids.KeyDID(sig["kid"].(string))

		verified, err := keyDid.Verify(cidz, sig["sig"].(string))
		if err != nil {
			return nil, err
		}

		if !verified {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("invalid sig")
		}
		verifiedDids = append(verifiedDids, keyDid)
	}

	cborgNode, err := cbornode.Decode(sTx.Tx, multihash.SHA2_256, -1)
	if err != nil {
		return nil, err
	}

	jsonData, err = cborgNode.MarshalJSON()
	if err != nil {
		return nil, err
	}

	txShell := VSCTransactionShell{}
	err = json.Unmarshal(jsonData, &txShell)
	if err != nil {
		return nil, err
	}

	for _, auth := range txShell.Headers.RequiredAuths {
		if !slices.Contains(verifiedDids, dids.KeyDID(auth)) {
			fmt.Println("Missing auth", auth)
			return nil, errors.New("missing required auth")
		}
	}

	err = tp.indexTx(cidz.String(), txShell)
	if err != nil {
		return nil, err
	}
	cidc, err := tp.datalayer.PutRaw(sTx.Tx, datalayer.PutRawOptions{
		Codec: multicodec.DagCbor,
	})
	if err != nil {
		return nil, err
	}

	fmt.Println("tx CID", cidz.String(), "cidc", cidc.String())

	fmt.Println("Options", options)
	if options[0].Broadcast {

		fmt.Println("cidz.String(), sTx", cidz.String(), sTx)
		err = tp.Broadcast(cidz.String(), sTx)
		if err != nil {
			return nil, err
		}
	}

	return &cidz, nil
}

func (tp *TransactionPool) Broadcast(id string, serializedTx SerializedVSCTransaction) error {
	b64tx := base64.StdEncoding.EncodeToString(serializedTx.Tx)
	b64sig := base64.StdEncoding.EncodeToString(serializedTx.Sig)

	return tp.service.Send(p2pMessage{
		Type: "announce_tx",
		Data: map[string]interface{}{
			"id":  id,
			"tx":  b64tx,
			"sig": b64sig,
		},
	})
}

func (tp *TransactionPool) ReceiveTx(p2pMsg p2pMessage) {
	if p2pMsg.Type != "announce_tx" {
		return
	}

	formattedData := struct {
		Tx  string `json:"tx"`
		Sig string `json:"sig"`
	}{}

	bmh, _ := json.Marshal(p2pMsg.Data)
	err := json.Unmarshal(bmh, &formattedData)

	if err != nil {
		return
	}

	decodedTx, _ := base64.StdEncoding.DecodeString(formattedData.Tx)
	decodedSig, _ := base64.StdEncoding.DecodeString(formattedData.Sig)

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(multihash.SHA2_256),
		MhLength: -1,
	}

	cidz, _ := prefix.Sum(decodedTx)

	txShell := VSCTransactionShell{}

	node, err := cbornode.Decode(decodedTx, multihash.SHA2_256, -1)

	bbytes, _ := node.MarshalJSON()

	json.Unmarshal(bbytes, &txShell)

	sigPack := SignaturePackage{}

	sigNode, err := cbornode.Decode(decodedSig, multihash.SHA2_256, -1)
	sigJson, _ := sigNode.MarshalJSON()

	json.Unmarshal(sigJson, &sigPack)

	verified, verifiedAuths, _ := VerifyTxSignatures(cidz, sigPack)

	if verified {
		for _, auth := range txShell.Headers.RequiredAuths {
			if !slices.Contains(verifiedAuths, auth) {
				fmt.Println("Missing auth", auth)
				return
			}
		}

		tp.indexTx(cidz.String(), txShell)
	}
}

func (tp *TransactionPool) indexTx(txId string, txShell VSCTransactionShell) error {
	payloadJson := map[string]interface{}{}

	err := cbornode.DecodeInto(txShell.Tx.Payload, &payloadJson)
	if err != nil {
		return err
	}
	//Ensure modified after to avoid overriden fields
	payloadJson["type"] = txShell.Tx.Type

	return tp.TxDb.Ingest(transactions.IngestTransactionUpdate{
		Id:            txId,
		RequiredAuths: txShell.Headers.RequiredAuths,
		Type:          "vsc",
		Version:       txShell.Version,
		Nonce:         txShell.Headers.Nonce,
		Tx:            payloadJson,
	})
}

func (tp *TransactionPool) Init() error {
	return nil
}

func (tp *TransactionPool) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		err := tp.startP2P()
		if err != nil {
			reject(err)
			return
		}

		<-tp.service.Context().Done()
		resolve(nil)
	})
}

func (tp *TransactionPool) Stop() error {
	return tp.stopP2P()
}

func New(p2p *libp2p.P2PServer, txDb transactions.Transactions, da *datalayer.DataLayer, conf common.IdentityConfig) *TransactionPool {
	return &TransactionPool{
		TxDb:      txDb,
		p2p:       p2p,
		datalayer: da,
		conf:      conf,
	}
}
