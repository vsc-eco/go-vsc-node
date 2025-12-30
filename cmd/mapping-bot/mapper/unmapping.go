package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"time"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mempool"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/hasura/go-graphql-client"
)

// SigningData and UnsignedSigHash structs same as contract
type SigningData struct {
	RawTx             string                     `json:"tx"`
	UnsignedSigHashes []database.UnsignedSigHash `json:"unsigned_sig_hashes"`
}

type HashMetadata struct {
	TxId  string
	Index uint32
}

type TxRawIdPair struct {
	RawTx string
	TxId  string
}

func (ms *MapperState) HandleUnmap(
	memPoolClient *mempool.MempoolClient,
) {
	ms.L.Debug("handling unmap")

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()

	txSpends, err := FetchTxSpends(ctx, ms.GqlClient)

	ms.ProcessTxSpends(ctx, ms.GqlClient, txSpends)
	finishedTxs, err := ms.CheckSignagures(ctx, ms.GqlClient)
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
			slog.Debug("request to be sent", "txId", tx.TxId, "rawTx", tx.RawTx)
			if !slog.Default().Enabled(ctx, slog.LevelDebug) {
				err := memPoolClient.PostTx(tx.RawTx)
				if err != nil {
					slog.Warn("transaction failed to post", "txId", tx.TxId)
					continue
				}
			}
			ms.Db.State.MarkTransactionSent(ctx, tx.TxId)
		}
	}
}

func (ms *MapperState) ProcessTxSpends(
	ctx context.Context,
	gqlClient *graphql.Client,
	incomingTxSpends map[string]*SigningData,
) {
	for txId, signingData := range incomingTxSpends {
		// already in the system

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

		err := ms.Db.State.AddPendingTransaction(ctx, txId, signingData.RawTx, signingData.UnsignedSigHashes)
		if err != nil && err != database.ErrPendingTxExists {

		}
	}
}

func (ms *MapperState) CheckSignagures(
	ctx context.Context,
	graphQlClient *graphql.Client,
) ([]*database.PendingTransaction, error) {
	allHashes, err := ms.Db.State.GetAllPendingSigHashes(ctx)
	if err != nil {
		return nil, err
	}

	newSignagutes, err := FetchSignatures(ctx, graphQlClient, allHashes)
	if err != nil {
		return nil, err
	}

	fullySignedTxs, err := ms.Db.State.UpdateSignatures(ctx, newSignagutes)
	if err != nil {
		return nil, err
	}

	return fullySignedTxs, nil
}

func attachSignatures(signedData *database.PendingTransaction) (*TxRawIdPair, error) {
	var tx wire.MsgTx
	txBytes, err := hex.DecodeString(signedData.RawTx)
	if err != nil {
		return nil, err
	}
	tx.Deserialize(bytes.NewReader(txBytes))

	for _, inputData := range signedData.Signatures {
		signature := append(signedData.Signatures[inputData.Index].Signature, byte(txscript.SigHashAll))

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
