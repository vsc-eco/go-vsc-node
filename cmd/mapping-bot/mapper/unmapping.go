package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
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
			if tx == nil || !postDue(tx.TxId) {
				continue
			}
			b.L.Debug("request to be sent", "txId", tx.TxId, "rawTx", tx.RawTx)
			if err := b.postTxWithRetry(tx.RawTx, 3); err != nil {
				notePostFailed(tx.TxId)
				noteInputsMissing(tx.TxId, err)
				b.L.Warn("transaction failed to post after retries; next attempt in "+postRetryInterval.String(), "err", err, "txId", tx.TxId)
				continue
			}
			clearPostFailed(tx.TxId)
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

	// These three reads gate every confirmSpend below: depth, already settled,
	// provable at all. A gate that could not be read has not passed. On testnet
	// 2026-10-06 a node restart made all three fail at once, the pass went ahead
	// and three spends mined below the prune floor were each refused on chain at
	// RC cost, leaving the bot short of what migrateVault needs. Skip the pass;
	// the next cycle reads again.
	lastContractHeightStr, err := b.gql().FetchLastHeight(ctx)
	if err != nil {
		b.L.Warn("skipping confirmSpend pass: contract height unreadable", "error", err)
		return
	}
	contractHeight, err := strconv.ParseUint(lastContractHeightStr, 10, 64)
	if err != nil {
		b.L.Warn("skipping confirmSpend pass: invalid contract height", "value", lastContractHeightStr)
		return
	}

	// A spend mined below the contract's prune floor can never be proven: its header
	// is gone, so confirmSpend fails on every cycle and burns RC until the 7-day
	// cleanup drops the tx. Skip it with one warning.
	pruneFloor, pruneFloorSet, err := b.gql().FetchPruneFloor(ctx)
	if err != nil {
		b.L.Warn("skipping confirmSpend pass: contract prune floor unreadable", "error", err)
		return
	}

	// The contract's pending spends: a sent tx missing from it has settled.
	pending, err := b.gql().FetchTxSpends(ctx)
	if err != nil {
		b.L.Warn("skipping confirmSpend pass: pending spends unreadable", "error", err)
		return
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

		// Wait until the confirmation block is as deep in the contract as the
		// contract requires, the same way HandleMap waits before mapping. A call
		// made earlier is refused and still costs RC (BOT-CONF-1).
		if contractHeight < b.contractHeightNeededFor(details.BlockHeight) {
			b.L.Info("delaying confirmSpend, block not yet deep enough in the contract",
				"txId", txId, "blockHeight", details.BlockHeight, "contractHeight", contractHeight)
			continue
		}

		// A spend the contract no longer lists as pending has settled (by this
		// bot's earlier call whose status poll timed out, or by another
		// submitter). Calling confirmSpend again only fails and costs RC: on
		// testnet 2026-10-06 the bot re-sent it every two minutes after the sweep
		// had settled. Record it confirmed and move on.
		if _, still := pending[txId]; !still {
			b.L.Info("spend already settled in the contract; marking it confirmed", "txId", txId)
			if err := b.stateDB().MarkTransactionConfirmed(ctx, txId); err != nil {
				b.L.Warn("failed to mark tx confirmed in DB", "txId", txId, "error", err)
			}
			continue
		}

		if pruneFloorSet && details.BlockHeight < pruneFloor {
			if warnUnprovableOnce(txId) {
				b.L.Warn("spend was mined below the contract's prune floor; it cannot be proven, so confirmSpend is skipped",
					"txId", txId, "blockHeight", details.BlockHeight, "pruneFloor", pruneFloor)
			}
			continue
		}

		b.L.Info("tx confirmed on chain, building proof for confirmSpend", "txId", txId)

		payload, err := b.buildConfirmSpendPayload(ctx, dbTx, details)
		if err != nil {
			b.L.Warn("failed to build confirmSpend payload", "txId", txId, "error", err)
			continue
		}

		if _, err := b.callWithRetry(ctx, payload, "confirmSpend", broadcastRetryAttempts); err != nil {
			b.L.Warn("confirmSpend failed", "txId", txId, "error", err)
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

// postRetryInterval spaces re-posts of a signed spend Bitcoin refused. A refusal
// that never clears (an input that does not exist, like the shared testnet vault's
// phantom legacy coin, or one already spent by a backup-key spend) was re-posted
// three times every cycle until the sweep was abandoned days later: thousands of
// requests to the same API the bot reads blocks and confirmations from.
const postRetryInterval = 5 * time.Minute

var (
	postFailMu   sync.Mutex
	postFailedAt = map[string]time.Time{}
	postNow      = time.Now
)

// postDue reports whether txId may be posted now: never refused, or refused at
// least postRetryInterval ago.
func postDue(txId string) bool {
	postFailMu.Lock()
	defer postFailMu.Unlock()
	at, ok := postFailedAt[txId]
	return !ok || postNow().Sub(at) >= postRetryInterval
}

func notePostFailed(txId string) {
	postFailMu.Lock()
	defer postFailMu.Unlock()
	postFailedAt[txId] = postNow()
}

func clearPostFailed(txId string) {
	postFailMu.Lock()
	defer postFailMu.Unlock()
	delete(postFailedAt, txId)
	delete(inputsMissing, txId)
}

// inputsMissing remembers spends Bitcoin refused because an input does not exist
// or is already spent. A fee bump cannot help those (BOT-REDRIVE-1).
var inputsMissing = map[string]bool{}

func noteInputsMissing(txId string, err error) {
	if err == nil || !strings.Contains(err.Error(), "missingorspent") {
		return
	}
	postFailMu.Lock()
	defer postFailMu.Unlock()
	inputsMissing[txId] = true
}

func refusedForMissingInputs(txId string) bool {
	postFailMu.Lock()
	defer postFailMu.Unlock()
	return inputsMissing[txId]
}

var (
	unprovableMu     sync.Mutex
	unprovableWarned = map[string]bool{}
)

// warnUnprovableOnce reports whether this is the first time txId was skipped as
// unprovable, so the operator gets one warning per tx rather than one per cycle.
func warnUnprovableOnce(txId string) bool {
	unprovableMu.Lock()
	defer unprovableMu.Unlock()
	if unprovableWarned[txId] {
		return false
	}
	unprovableWarned[txId] = true
	return true
}
