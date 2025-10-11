package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"vsc-node/cmd/mapping-bot/mempool"

	"github.com/btcsuite/btcd/wire"
)

// SigningData and UnsignedSigHash structs same as contract
type SigningData struct {
	Tx                string            `json:"tx"`
	UnsignedSigHashes []UnsignedSigHash `json:"unsigned_sig_hashes"`
}

type UnsignedSigHash struct {
	Index         uint32 `json:"index"`
	SigHash       []byte `json:"sig_hash"`
	WitnessScript []byte `json:"witness_script"`
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

func (ms *MapperState) HandleUnmap(client *mempool.MempoolClient, txSpends map[string]*SigningData) {
	ms.ProcessTxSpends(txSpends)
	finishedTxs := ms.CheckSignagures()

	if len(finishedTxs) > 0 {
		txPairs := make([]*TxRawIdPair, len(finishedTxs))
		for i, signedData := range finishedTxs {
			txPair, err := attachSignatures(signedData)
			// can just log the error and continue, because it will just refetch from contract
			// state and try to compile it again
			fmt.Printf("Error attaching signatures to transaction with ID %s.\n", err.Error())
			txPairs[i] = txPair
		}
		for _, tx := range txPairs {
			client.PostTx(tx.RawTx)
			ms.SentTxs[tx.TxId] = true
		}
	}

	// store data to datastore
	ms.Mutex.Lock()
	defer ms.Mutex.Unlock()
	observedTxsJson, err := json.Marshal(ms.ObservedTxs)
	if err != nil {
		return
	}
	ms.FfsDatastore.Put(context.TODO(), observedTxsKey, observedTxsJson)
	sentTxsJson, err := json.Marshal(ms.SentTxs)
	if err != nil {
		return
	}
	ms.FfsDatastore.Put(context.TODO(), sentTxsKey, sentTxsJson)
}

func (ms *MapperState) ProcessTxSpends(incomingTxSpends map[string]*SigningData) {
	ms.Mutex.Lock()
	defer ms.Mutex.Unlock()
	for txId, signingData := range incomingTxSpends {
		_, ok := ms.ObservedTxs[txId]
		if ok {
			continue
		}
		_, ok = ms.AwaitingSignatureTxs.Txs[txId]
		if ok {
			continue
		}
		ms.AwaitingSignatureTxs.Txs[txId] = &SignedData{
			Tx:                signingData.Tx,
			UnsignedSigHashes: signingData.UnsignedSigHashes,
			Signatures:        make([][]byte, len(signingData.UnsignedSigHashes)),
			TotalSignatures:   uint32(len(signingData.UnsignedSigHashes)),
			CurrentSignatures: 0,
		}
		for _, hash := range signingData.UnsignedSigHashes {
			hashHex := hex.EncodeToString(hash.SigHash)
			ms.AwaitingSignatureTxs.Hashes[hashHex] = &HashMetadata{
				TxId:  txId,
				Index: hash.Index,
			}
		}
	}
}

func (ms *MapperState) CheckSignagures() []*SignedData {
	ms.Mutex.Lock()
	allHashes := make([]string, len(ms.AwaitingSignatureTxs.Hashes))
	i := 0
	for hash := range ms.AwaitingSignatureTxs.Hashes {
		allHashes[i] = hash
		i++
	}
	ms.Mutex.Unlock()

	newSignagutes := FetchSignatures(allHashes)

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

	return fullySignedTxs
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

		witness := wire.TxWitness{
			signature[:],
			inputData.WitnessScript,
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
