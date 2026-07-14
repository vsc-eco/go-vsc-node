package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
			err := b.postTxWithRetry(tx.RawTx, 3)
			// "Already in chain" means a broadcast we (or another bot instance)
			// made earlier already landed the tx. Treat it as a successful send:
			// mark it sent so HandleConfirmations picks it up and fires
			// confirmSpend once the confirmation block is ingested — no need to
			// fast-track anything, the normal confirmation path handles it.
			if err != nil && !errors.Is(err, chain.ErrTxAlreadyInChain) {
				b.L.Warn("transaction failed to post after retries", "err", err, "txId", tx.TxId)
				continue
			}
			if errors.Is(err, chain.ErrTxAlreadyInChain) {
				b.L.Info("tx already on-chain, marking sent", "txId", tx.TxId)
			}
			height, _ := b.LastBlock()
			if err := b.stateDB().MarkTransactionSent(ctx, tx.TxId, height); err != nil &&
				!errors.Is(err, database.ErrTxNotFound) {
				b.L.Warn("failed to mark transaction sent", "err", err, "txId", tx.TxId)
			}
		}
	}
}

// HandleConfirmations drives each broadcast BTC withdrawal to a confirmed
// on-contract state. For each "sent" tx that has confirmed on-chain it either
// broadcasts a confirmSpend or, if one is already in flight, polls that VSC tx's
// status across cycles.
//
// The polling model matters: a confirmSpend is a contract call, so it is only
// "INCLUDED" when its block is ingested and does not reach "CONFIRMED" until the
// slot executes/finalizes — which routinely takes longer than a single
// HandleConfirmations pass. Blocking-waiting for CONFIRMED therefore times out
// even when the confirmSpend succeeds, and a naive re-broadcast then reverts
// ("no unconfirmed outputs matched" — the change UTXO is already promoted),
// which would look like a failure and eventually be abandoned. Broadcasting once
// and polling the recorded VSC tx id avoids both: success is recognized whenever
// it lands, and we never re-broadcast a confirmSpend that already succeeded.
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

	// Lazily fetch the contract's pending spends once per cycle — only needed to
	// disambiguate a reverted confirmSpend (already resolved vs. genuinely failed).
	var pendingSpends map[string]*contractinterface.SigningData
	var pendingFetched bool
	getPending := func() map[string]*contractinterface.SigningData {
		if !pendingFetched {
			pendingFetched = true
			ps, perr := b.gql().FetchTxSpends(ctx)
			if perr != nil {
				b.L.Debug("failed to fetch pending spends for confirmSpend check", "error", perr)
				ps = nil
			}
			pendingSpends = ps
		}
		return pendingSpends
	}

	for _, dbTx := range sentTxs {
		txId := dbTx.TxID
		now := time.Now().UTC()

		// Anti-stuck guard: once the confirmSpend retry window is exhausted we
		// stop resubmitting. The tx has been escalated (Error log + /health) and
		// needs operator attention, so don't keep hammering the contract.
		if dbTx.ConfirmAbandoned {
			continue
		}

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

		// A confirmSpend is already in flight — poll ITS status instead of
		// broadcasting a duplicate. This branch runs every cycle (no backoff
		// gate) so success is detected as soon as it finalizes.
		if dbTx.ConfirmSpendVscTxId != "" {
			b.pollInFlightConfirmSpend(ctx, dbTx, now, getPending)
			continue
		}

		// No confirmSpend in flight. Gate re-broadcasts behind the backoff so a
		// genuinely-failing confirmSpend isn't hammered every cycle.
		if dbTx.NextConfirmAttemptAt != nil && now.Before(*dbTx.NextConfirmAttemptAt) {
			continue
		}

		b.L.Info("tx confirmed on chain, building proof for confirmSpend", "txId", txId)

		payload, err := b.buildConfirmSpendPayload(ctx, dbTx, details)
		if err != nil {
			b.L.Warn("failed to build confirmSpend payload", "txId", txId, "error", err)
			b.recordConfirmSpendFailure(ctx, dbTx, now, "build payload: "+err.Error())
			continue
		}

		vscTxId, err := b.caller().CallContract(ctx, payload, "confirmSpend")
		if err != nil {
			b.recordConfirmSpendFailure(ctx, dbTx, now, "broadcast: "+err.Error())
			continue
		}

		b.recordConfirmSpendBroadcast(ctx, dbTx, now, vscTxId)
		b.L.Info("confirmSpend broadcast, awaiting confirmation", "txId", txId, "vscTx", vscTxId)
	}
}

// pollInFlightConfirmSpend checks the status of the confirmSpend VSC tx recorded
// for dbTx and advances its state:
//   - CONFIRMED/PROCESSED → the spend is confirmed on the contract; mark done.
//   - FAILED → the call reverted. If the spend is no longer pending on the
//     contract it was already resolved (a prior confirmSpend, or the contract's
//     own auto-confirmation) → mark done; otherwise record a failure and
//     re-broadcast on a later cycle.
//   - anything else / query error → still resolving; keep polling (abandon only
//     once the overall retry window elapses).
func (b *Bot) pollInFlightConfirmSpend(
	ctx context.Context,
	dbTx database.Transaction,
	now time.Time,
	getPending func() map[string]*contractinterface.SigningData,
) {
	txId := dbTx.TxID
	status, err := b.gql().FetchTransactionStatus(ctx, dbTx.ConfirmSpendVscTxId)
	if err != nil {
		// Not indexed yet, or a transient query error — keep waiting.
		b.recordConfirmSpendPending(ctx, dbTx, now, "status query: "+err.Error())
		return
	}

	switch status {
	case "CONFIRMED", "PROCESSED":
		if err := b.stateDB().MarkTransactionConfirmed(ctx, txId); err != nil {
			b.L.Warn("failed to mark tx confirmed in DB", "txId", txId, "error", err)
			return
		}
		b.L.Info("confirmSpend confirmed", "txId", txId, "vscTx", dbTx.ConfirmSpendVscTxId)
	case "FAILED":
		// The confirmSpend reverted. If the contract no longer lists this tx as a
		// pending spend, its change UTXOs were already promoted (by a prior
		// confirmSpend or the contract's own map-time auto-confirmation), so treat
		// it as done rather than re-broadcasting a call that can only revert again.
		if ps := getPending(); ps != nil {
			if _, stillPending := ps[txId]; !stillPending {
				if err := b.stateDB().MarkTransactionConfirmed(ctx, txId); err != nil {
					b.L.Warn("failed to mark tx confirmed in DB", "txId", txId, "error", err)
					return
				}
				b.L.Info("confirmSpend already resolved on contract, marking confirmed", "txId", txId)
				return
			}
		}
		b.recordConfirmSpendFailure(ctx, dbTx, now, "confirmSpend reverted (FAILED)")
	default:
		// UNCONFIRMED / INCLUDED — executed inclusion pending finalization.
		b.recordConfirmSpendPending(ctx, dbTx, now, "awaiting confirmSpend status "+status)
	}
}

// recordConfirmSpendBroadcast records a freshly broadcast confirmSpend: it stores
// the in-flight VSC tx id (polled on subsequent cycles) and anchors the give-up
// window on the first broadcast. It does not advance the attempt/backoff counter
// — those track re-broadcasts after failures.
func (b *Bot) recordConfirmSpendBroadcast(ctx context.Context, dbTx database.Transaction, now time.Time, vscTxId string) {
	first := now
	if dbTx.FirstConfirmAttemptAt != nil {
		first = *dbTx.FirstConfirmAttemptAt
	}
	if err := b.stateDB().SetConfirmSpendRetry(ctx, dbTx.TxID, database.ConfirmSpendRetry{
		Attempts:       dbTx.ConfirmAttempts,
		FirstAttemptAt: first,
		NextAttemptAt:  now.Add(confirmSpendBackoff(dbTx.ConfirmAttempts + 1)),
		VscTxId:        vscTxId,
	}); err != nil {
		b.L.Warn("failed to record confirmSpend broadcast", "txId", dbTx.TxID, "error", err)
	}
}

// recordConfirmSpendPending is called while an in-flight confirmSpend is still
// resolving. It writes nothing unless the overall retry window has elapsed, in
// which case the tx is abandoned (an in-flight tx that never finalizes) and
// surfaced on /health.
func (b *Bot) recordConfirmSpendPending(ctx context.Context, dbTx database.Transaction, now time.Time, reason string) {
	first := now
	if dbTx.FirstConfirmAttemptAt != nil {
		first = *dbTx.FirstConfirmAttemptAt
	}
	if now.Before(first.Add(confirmSpendGiveUpAfter)) {
		// Still within the window — keep polling next cycle, nothing to persist.
		return
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	if err := b.stateDB().SetConfirmSpendRetry(ctx, dbTx.TxID, database.ConfirmSpendRetry{
		Attempts:       dbTx.ConfirmAttempts,
		FirstAttemptAt: first,
		NextAttemptAt:  now,
		Abandoned:      true,
		LastError:      reason,
		VscTxId:        dbTx.ConfirmSpendVscTxId,
	}); err != nil {
		b.L.Warn("failed to record confirmSpend abandonment", "txId", dbTx.TxID, "error", err)
	}
	b.L.Error("confirmSpend abandoned; in-flight tx never finalized, needs operator attention",
		"txId", dbTx.TxID, "vscTx", dbTx.ConfirmSpendVscTxId, "window", confirmSpendGiveUpAfter.String(), "reason", reason)
}

// recordConfirmSpendFailure applies the exponential-backoff / give-up bookkeeping
// after a confirmSpend build/broadcast failure or a reverted (FAILED) call. It
// clears the in-flight VSC tx id so the next eligible cycle re-broadcasts, and
// once the retry window (confirmSpendGiveUpAfter) elapses since the first attempt
// the tx is marked abandoned and surfaced on /health.
func (b *Bot) recordConfirmSpendFailure(ctx context.Context, dbTx database.Transaction, now time.Time, errMsg string) {
	attempts := dbTx.ConfirmAttempts + 1
	first := now
	if dbTx.FirstConfirmAttemptAt != nil {
		first = *dbTx.FirstConfirmAttemptAt
	}
	backoff := confirmSpendBackoff(attempts)
	abandoned := !now.Before(first.Add(confirmSpendGiveUpAfter))

	// Keep the persisted error bounded — it is surfaced verbatim on /health.
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}

	if err := b.stateDB().SetConfirmSpendRetry(ctx, dbTx.TxID, database.ConfirmSpendRetry{
		Attempts:       attempts,
		FirstAttemptAt: first,
		NextAttemptAt:  now.Add(backoff),
		Abandoned:      abandoned,
		LastError:      errMsg,
		VscTxId:        "", // clear so the next eligible cycle re-broadcasts
	}); err != nil {
		b.L.Warn("failed to record confirmSpend retry state", "txId", dbTx.TxID, "error", err)
	}

	if abandoned {
		b.L.Error("confirmSpend abandoned after exhausting retry window; needs operator attention",
			"txId", dbTx.TxID, "attempts", attempts, "window", confirmSpendGiveUpAfter.String(), "lastError", errMsg)
	} else {
		b.L.Warn("confirmSpend failed, backing off",
			"txId", dbTx.TxID, "attempts", attempts, "nextAttemptIn", backoff.String(), "error", errMsg)
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

	// confirmSpend's `indices` are OUTPUT vouts: the contract promotes its own
	// unconfirmed change UTXOs (identified by txid + vout) to the confirmed pool.
	// It filters the supplied indices against the change UTXOs it actually tracks
	// for this txid, so listing every output index is both safe and correct —
	// only genuine change outputs are promoted; the withdrawal destination output
	// (untracked) is ignored.
	//
	// Previously this sent INPUT signature indices, which only happened to overlap
	// a change vout for multi-input spends. A single-input withdrawal sent [0]
	// while its change output sits at vout 1, so nothing matched and the contract
	// reverted with "no unconfirmed outputs matched the provided indices" —
	// leaving the tx in "sent" state and resubmitting confirmSpend every cycle
	// forever. Deriving indices from the tx's actual outputs removes that
	// divergence between the bot's payload and the on-chain transaction.
	var msgTx wire.MsgTx
	if err := msgTx.Deserialize(bytes.NewReader(dbTx.RawTx)); err != nil {
		return nil, fmt.Errorf("failed to deserialize stored raw tx for confirmSpend indices: %w", err)
	}
	indices := make([]uint32, len(msgTx.TxOut))
	for i := range msgTx.TxOut {
		indices[i] = uint32(i)
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
