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
func (tp *TransactionPool) IngestTx(sTx SerializedVSCTransaction, options ...IngestOptions) error {
	if sTx.Sig == nil {
		return errors.New("No signature provided")
	}

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(multihash.SHA2_256),
		MhLength: -1,
	}

	cidz, _ := prefix.Sum(sTx.Tx)

	tempObj := map[string]interface{}{}

	node, _ := cbornode.Decode(sTx.Sig, multihash.SHA2_256, -1)

	jsonData, _ := node.MarshalJSON()

	json.Unmarshal(jsonData, &tempObj)

	verifiedDids := make([]dids.KeyDID, 0)

	for _, v := range tempObj["sigs"].([]interface{}) {
		sig := v.(map[string]interface{})

		if sig["alg"] != "EdDSA" {
			return errors.New("Unsupported signature algorithm")
		}

		if sig["kid"] == nil {
			return errors.New("No key id provided")
		}

		if sig["sig"] == nil {
			return errors.New("No signature provided")
		}

		keyDid := dids.KeyDID(sig["kid"].(string))

		verified, err := keyDid.Verify(cidz, sig["sig"].(string))

		if !verified {
			return err
		}
		verifiedDids = append(verifiedDids, keyDid)
	}

	cborgNode, _ := cbornode.Decode(sTx.Tx, multihash.SHA2_256, -1)

	jsonData, _ = cborgNode.MarshalJSON()

	txShell := VSCTransactionShell{}
	json.Unmarshal(jsonData, &txShell)

	for _, auth := range txShell.Headers.RequiredAuths {
		if !slices.Contains(verifiedDids, dids.KeyDID(auth)) {
			fmt.Println("Missing auth", auth)
			return errors.New("missing required auth")
		}
	}

	tp.indexTx(cidz.String(), txShell)
	cidc, _ := tp.datalayer.PutRaw(sTx.Tx, datalayer.PutRawOptions{
		Codec: multicodec.DagCbor,
	})

	fmt.Println("tx CID", cidz.String(), "cidc", cidc.String())

	fmt.Println("Options", options)
	if options[0].Broadcast {

		fmt.Println("cidz.String(), sTx", cidz.String(), sTx)
		tp.Broadcast(cidz.String(), sTx)
	}

	return nil
}

func (tp *TransactionPool) Broadcast(id string, serializedTx SerializedVSCTransaction) {
	b64tx := base64.StdEncoding.EncodeToString(serializedTx.Tx)
	b64sig := base64.StdEncoding.EncodeToString(serializedTx.Sig)

	tp.service.Send(p2pMessage{
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

func (tp *TransactionPool) indexTx(txId string, txShell VSCTransactionShell) {
	payloadJson := map[string]interface{}{}

	cbornode.DecodeInto(txShell.Tx.Payload, &payloadJson)

	//Ensure modified after to avoid overriden fields
	payloadJson["type"] = txShell.Tx.Type

	tp.TxDb.Ingest(transactions.IngestTransactionUpdate{
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
