package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
	"vsc-node/cmd/mapping-bot/chain"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// ConfirmSpendParams is the payload for the confirmSpend contract action.
type ConfirmSpendParams struct {
	TxData  *VerificationRequest `json:"tx_data"`
	Indices []uint32             `json:"indices"`
}

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
			height, _ := b.LastBlock()
			b.stateDB().MarkTransactionSent(ctx, tx.TxId, height)
		}
	}
}

// HandleConfirmations checks all sent transactions against the blockchain.
// When a tx is confirmed, it builds a merkle proof and calls the mapping
// contract's confirmSpend action with a ConfirmSpendParams payload.
func (b *Bot) HandleConfirmations() {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sentTxs, err := b.stateDB().GetSentTransactions(ctx)
	if err != nil {
		b.L.Warn("failed to get sent transactions", "error", err)
		return
	}
	if len(sentTxs) == 0 {
		return
	}

	// Fetch the contract's last processed block height once, so we can check
	// whether each confirmation block has been ingested before calling confirmSpend.
	var contractHeight uint64
	var contractHeightOk bool
	lastContractHeightStr, err := b.gql().FetchLastHeight(ctx)
	if err != nil {
		b.L.Debug("failed to fetch contract height for confirmSpend", "error", err)
	} else {
		h, parseErr := strconv.ParseUint(lastContractHeightStr, 10, 64)
		if parseErr != nil {
			b.L.Debug("invalid contract height response", "value", lastContractHeightStr)
		} else {
			contractHeight = h
			contractHeightOk = true
		}
	}

	for _, dbTx := range sentTxs {
		txId := dbTx.TxID

		details, err := b.Chain.Client.GetTxDetails(txId)
		if err != nil {
			b.L.Debug("failed to check tx details", "txId", txId, "error", err)
			continue
		}
		if !details.Confirmed {
			continue
		}

		// Wait until the contract has processed the confirmation block,
		// the same way HandleMap waits before mapping.
		if contractHeightOk && contractHeight < details.BlockHeight {
			b.L.Info("delaying confirmSpend, block not yet in contract",
				"txId", txId, "blockHeight", details.BlockHeight, "contractHeight", contractHeight)
			continue
		}

		b.L.Info("tx confirmed on chain, building proof for confirmSpend", "txId", txId)

		payload, err := b.buildConfirmSpendPayload(ctx, dbTx, details)
		if err != nil {
			b.L.Warn("failed to build confirmSpend payload", "txId", txId, "error", err)
			continue
		}

		if err := b.callWithRetry(ctx, payload, "confirmSpend", 3); err != nil {
			b.L.Warn("confirmSpend failed after retries", "txId", txId, "error", err)
			continue
		}

		if err := b.stateDB().MarkTransactionConfirmed(ctx, txId); err != nil {
			b.L.Warn("failed to mark tx confirmed in DB", "txId", txId, "error", err)
			continue
		}

		b.L.Info("tx confirmed and confirmSpend called", "txId", txId)
	}
}

// buildConfirmSpendPayload constructs the JSON-encoded ConfirmSpendParams for a confirmed BTC tx.
// It fetches the raw block, builds a merkle proof, and collects the input indices
// that were signed (i.e. the VSC-mapped UTXOs being spent).
func (b *Bot) buildConfirmSpendPayload(
	ctx context.Context,
	dbTx database.Transaction,
	details chain.TxConfirmationDetails,
) ([]byte, error) {
	rawBlock, err := b.Chain.Client.GetRawBlock(details.BlockHash)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch raw block %s: %w", details.BlockHash, err)
	}

	var msgBlock wire.MsgBlock
	if err := msgBlock.Deserialize(bytes.NewReader(rawBlock)); err != nil {
		return nil, fmt.Errorf("failed to deserialize block: %w", err)
	}

	merkleProofHex, err := generateMerkleProof(&msgBlock, int(details.TxIndex))
	if err != nil {
		return nil, fmt.Errorf("failed to generate merkle proof: %w", err)
	}

	rawTxHex := hex.EncodeToString(dbTx.RawTx)

	// Collect the input indices that correspond to VSC-mapped UTXOs.
	indices := make([]uint32, 0, len(dbTx.Signatures))
	seen := make(map[uint32]struct{})
	for _, sig := range dbTx.Signatures {
		idx := uint32(sig.Index)
		if _, ok := seen[idx]; !ok {
			seen[idx] = struct{}{}
			indices = append(indices, idx)
		}
	}

	params := ConfirmSpendParams{
		TxData: &VerificationRequest{
			BlockHeight:    details.BlockHeight,
			RawTxHex:       rawTxHex,
			MerkleProofHex: merkleProofHex,
			TxIndex:        uint64(details.TxIndex),
		},
		Indices: indices,
	}
	return json.Marshal(params)
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
	// First, pick up any pending transactions that are already fully signed
	// but were never broadcast (e.g., due to a crash after the last signature was applied).
	alreadySigned, err := b.stateDB().GetFullySignedPendingTransactions(ctx)
	if err != nil {
		return nil, err
	}

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

	// Merge, deduplicating by TxID
	seen := make(map[string]struct{}, len(fullySignedTxs))
	for _, tx := range fullySignedTxs {
		seen[tx.TxID] = struct{}{}
	}
	for _, tx := range alreadySigned {
		if _, ok := seen[tx.TxID]; !ok {
			fullySignedTxs = append(fullySignedTxs, tx)
		}
	}

	return fullySignedTxs, nil
}

func attachSignatures(signedData *database.Transaction) (*TxRawIdPair, error) {
	var tx wire.MsgTx
	tx.Deserialize(bytes.NewReader(signedData.RawTx))

	for _, inputData := range signedData.Signatures {
		sig := signedData.Signatures[inputData.Index].Signature
		signature := make([]byte, len(sig)+1)
		copy(signature, sig)
		signature[len(sig)] = byte(txscript.SigHashAll)

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
