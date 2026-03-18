package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type HashMetadata struct {
	TxId  string
	Index uint32
}

type TxRawIdPair struct {
	RawTx string
	TxId  string
}

func (b *Bot) HandleUnmap() {
	b.L.Debug("handling unmap")

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()

	txSpends, err := b.gql().FetchTxSpends(ctx)
	if err != nil {
		b.L.Debug("failed to fetch tx spends from contract", "error", err)
	} else {
		b.L.Debug("fetched tx spends from contract", "count", len(txSpends))
	}

	b.ProcessTxSpends(ctx, txSpends)
	finishedTxs, err := b.CheckSignagures(ctx)
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
			b.L.Debug("request to be sent", "txId", tx.TxId, "rawTx", tx.RawTx)
			if err := b.postTxWithRetry(tx.RawTx, 3); err != nil {
				b.L.Warn("transaction failed to post after retries", "err", err, "txId", tx.TxId)
				continue
			}
			b.stateDB().MarkTransactionSent(ctx, tx.TxId)
		}
	}
}

// HandleConfirmations checks all sent transactions against the blockchain.
// When a tx is confirmed, it transitions the DB state to "confirmed" and calls
// the mapping contract's confirmSpend action to promote unconfirmed UTXOs.
func (b *Bot) HandleConfirmations() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sentTxIDs, err := b.stateDB().GetSentTransactionIDs(ctx)
	if err != nil {
		b.L.Warn("failed to get sent transactions", "error", err)
		return
	}
	if len(sentTxIDs) == 0 {
		return
	}

	for _, txId := range sentTxIDs {
		confirmed, err := b.Chain.Client.GetTxStatus(txId)
		if err != nil {
			b.L.Debug("failed to check tx status", "txId", txId, "error", err)
			continue
		}
		if !confirmed {
			continue
		}

		b.L.Info("tx confirmed on chain, calling confirmSpend", "txId", txId)

		// Call confirmSpend on the mapping contract to promote unconfirmed UTXOs
		payload := json.RawMessage(fmt.Sprintf(`{"tx_id":"%s"}`, txId))
		if err := b.callWithRetry(ctx, payload, "confirmSpend", 3); err != nil {
			b.L.Warn("confirmSpend failed after retries", "txId", txId, "error", err)
			continue
		}

		// Transition to confirmed in local DB
		err = b.stateDB().MarkTransactionConfirmed(ctx, txId)
		if err != nil {
			b.L.Warn("failed to mark tx confirmed in DB", "txId", txId, "error", err)
			continue
		}

		b.L.Info("tx confirmed and confirmSpend called", "txId", txId)
	}
}

func (b *Bot) ProcessTxSpends(
	ctx context.Context,
	incomingTxSpends map[string]*contractinterface.SigningData,
) {
	for txId, signingData := range incomingTxSpends {
		b.L.Debug("processing incoming tx spend", "txId", txId, "sigHashCount", len(signingData.UnsignedSigHashes))

		processed, err := b.stateDB().IsTransactionProcessed(ctx, txId)
		if err != nil {
			b.L.Debug("failed to check tx status", "txId", txId, "error", err)
			continue
		}
		if processed {
			b.L.Debug("tx spend already processed, skipping", "txId", txId)
			continue
		}

		err = b.stateDB().AddPendingTransaction(ctx, txId, signingData.Tx, signingData.UnsignedSigHashes)
		if err == database.ErrTxExists {
			b.L.Debug("tx spend already pending, skipping", "txId", txId)
		} else if err != nil {
			b.L.Debug("failed to add pending transaction", "txId", txId, "error", err)
		} else {
			b.L.Debug("added new pending transaction", "txId", txId)
		}
	}
}

func (b *Bot) CheckSignagures(
	ctx context.Context,
) ([]*database.Transaction, error) {
	allHashes, err := b.stateDB().GetAllPendingSigHashes(ctx)
	if err != nil {
		return nil, err
	}

	newSignagutes, err := b.gql().FetchSignatures(ctx, allHashes)
	if err != nil {
		return nil, err
	}

	fullySignedTxs, err := b.stateDB().UpdateSignatures(ctx, newSignagutes)
	if err != nil {
		return nil, err
	}

	return fullySignedTxs, nil
}

func attachSignatures(signedData *database.Transaction) (*TxRawIdPair, error) {
	var tx wire.MsgTx
	tx.Deserialize(bytes.NewReader(signedData.RawTx))

	for _, inputData := range signedData.Signatures {
		signature := append(signedData.Signatures[inputData.Index].Signature, byte(txscript.SigHashAll))

		branchSelector := []byte{0x01} // primary key path (OP_IF)
		if inputData.IsBackup {
			branchSelector = []byte{} // backup key path (OP_ELSE)
		}
		witness := wire.TxWitness{
			signature[:],
			branchSelector,
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
