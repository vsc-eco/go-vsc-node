package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"vsc-node/cmd/mapping-bot/mempool"

	"github.com/btcsuite/btcd/wire"
	"github.com/hasura/go-graphql-client"
)

// SigningData and UnsignedSigHash structs same as contract
type SigningData struct {
	Tx                string            `json:"tx"`
	UnsignedSigHashes []UnsignedSigHash `json:"unsigned_sig_hashes"`
}

type UnsignedSigHash struct {
	Index         uint32 `json:"index"`
	SigHash       string `json:"sig_hash"`
	WitnessScript string `json:"witness_script"`
}

type HashMetadata struct {
	TxId  string
	Index uint32
}

type SignedData struct {
	Tx                string
	UnsignedSigHashes []UnsignedSigHash
	Signatures        [][]byte
	TotalSignatures   uint32
	CurrentSignatures uint32
}

type AwaitingSignature struct {
	Txs    map[string]*SignedData
	Hashes map[string]*HashMetadata
}

type TxRawIdPair struct {
	RawTx string
	TxId  string
}

func (ms *MapperState) HandleUnmap(
	memPoolClient *mempool.MempoolClient,
	txSpends map[string]*SigningData,
) {
	ms.ProcessTxSpends(ms.GqlClient, txSpends)
	finishedTxs, err := ms.CheckSignagures(ms.GqlClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error fetching signatures from the database: %s", err.Error())
		return
	}

	if len(finishedTxs) > 0 {
		txPairs := make([]*TxRawIdPair, len(finishedTxs))
		for i, signedData := range finishedTxs {
			txPair, err := attachSignatures(signedData)
			// can just log the error and continue, because it will just refetch from contract
			// state and try to compile it again
			if err != nil {
				fmt.Fprintf(os.Stderr, "error attaching signatures to transaction with id: %s\n", err.Error())
			}
			txPairs[i] = txPair
		}
		for _, tx := range txPairs {
			memPoolClient.PostTx(tx.RawTx)
			ms.SentTxs[tx.TxId] = true
		}
	}

	// store data to datastore
	ms.Mutex.Lock()
	defer ms.Mutex.Unlock()
	sentTxsJson, err := json.Marshal(ms.SentTxs)
	if err != nil {
		return
	}
	ms.FfsDatastore.Put(context.TODO(), sentTxsKey, sentTxsJson)
}

func (ms *MapperState) ProcessTxSpends(gqlClient *graphql.Client, incomingTxSpends map[string]*SigningData) {
	ms.Mutex.Lock()
	defer ms.Mutex.Unlock()
	for txId, signingData := range incomingTxSpends {
		// already in the system
		_, ok := ms.AwaitingSignatureTxs.Txs[txId]
		if ok {
			continue
		}

		// could re-enable this, but it does a lot of gql requests for probably no reason
		// make sure none of the tx's utxos are observed before adding to the system
		// this should never happen
		// exists := false
		// for i := range signingData.UnsignedSigHashes {
		// 	ok, err := FetchObservedTx(gqlClient, txId, i)
		// 	if ok || err != nil {
		// 		exists = true
		// 		break
		// 	}
		// }
		// if exists {
		// 	continue
		// }

		for _, hash := range signingData.UnsignedSigHashes {
			ms.AwaitingSignatureTxs.Hashes[hash.SigHash] = &HashMetadata{
				TxId:  txId,
				Index: hash.Index,
			}
		}

		ms.AwaitingSignatureTxs.Txs[txId] = &SignedData{
			Tx:                signingData.Tx,
			UnsignedSigHashes: signingData.UnsignedSigHashes,
			Signatures:        make([][]byte, len(signingData.UnsignedSigHashes)),
			TotalSignatures:   uint32(len(signingData.UnsignedSigHashes)),
			CurrentSignatures: 0,
		}
	}
}

func (ms *MapperState) CheckSignagures(graphQlClient *graphql.Client) ([]*SignedData, error) {
	ms.Mutex.Lock()
	allHashes := make([]string, len(ms.AwaitingSignatureTxs.Hashes))
	i := 0
	for hash := range ms.AwaitingSignatureTxs.Hashes {
		allHashes[i] = hash
		i++
	}
	ms.Mutex.Unlock()

	newSignagutes, err := FetchSignatures(graphQlClient, allHashes)
	if err != nil {
		return nil, err
	}

	ms.Mutex.Lock()
	defer ms.Mutex.Unlock()

	fullySignedTxs := make([]*SignedData, 0)
	for hash, sig := range newSignagutes {
		metadata := ms.AwaitingSignatureTxs.Hashes[hash]
		signedData := ms.AwaitingSignatureTxs.Txs[metadata.TxId]
		signedData.Signatures[metadata.Index] = sig
		signedData.CurrentSignatures++
		delete(ms.AwaitingSignatureTxs.Hashes, hash)

		// should never be greater but just in case
		if signedData.CurrentSignatures >= signedData.TotalSignatures {
			fullySignedTxs = append(fullySignedTxs, signedData)
			delete(ms.AwaitingSignatureTxs.Txs, metadata.TxId)
		}
	}

	return fullySignedTxs, nil
}

func attachSignatures(signedData *SignedData) (*TxRawIdPair, error) {
	var tx wire.MsgTx
	txBytes, err := hex.DecodeString(signedData.Tx)
	if err != nil {
		return nil, err
	}
	tx.Deserialize(bytes.NewReader(txBytes))

	for _, inputData := range signedData.UnsignedSigHashes {
		signature := signedData.Signatures[inputData.Index]

		witnessScriptBytes, err := hex.DecodeString(inputData.WitnessScript)
		if err != nil {
			return nil, err
		}

		witness := wire.TxWitness{
			signature[:],
			witnessScriptBytes,
		}

		tx.TxIn[inputData.Index].Witness = witness
	}

	var buf bytes.Buffer
	// serialize is almost the same but with a different protocol version. Not sure if that
	// actually changes the result
	if err := tx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding); err != nil {
		return nil, err
	}

	return &TxRawIdPair{
		RawTx: hex.EncodeToString(buf.Bytes()),
		TxId:  tx.TxID(),
	}, nil
}
