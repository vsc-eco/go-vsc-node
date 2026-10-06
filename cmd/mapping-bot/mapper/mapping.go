package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/wire"
)

// dropHeightDiff is set per-chain via ChainConfig.DropHeightDiff

// HandleMap processes a single block for mapping transactions.
// Returns true if the block was processed, false if skipped (e.g., contract not ready).
func (b *Bot) HandleMap(
	blockBytes []byte,
	blockHeight uint64,
) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	lastContractHeightStr, err := b.gql().FetchLastHeight(ctx)
	if err != nil {
		b.L.Error("error fetching contract's last block height", "err", err)
		return false
	}
	lastContractHeight, err := strconv.ParseUint(lastContractHeightStr, 10, 32)
	if err != nil {
		b.L.Error("response received for last contract height is not an integer", "value", lastContractHeightStr)
		return false
	}

	if lastContractHeight < b.contractHeightNeededFor(blockHeight) {
		b.L.Info("delaying processing, block not yet deep enough in the contract",
			"blockHeight", blockHeight, "contractHeight", lastContractHeight,
			"confirmationsRequired", b.Chain.ConfirmationsRequired)
		return false
	}

	foundTxs, err := b.ParseBlock(ctx, blockBytes, blockHeight)
	if err != nil {
		b.L.Error("error parsing block", "err", err)
		return false
	}

	jsonMessages := make([]json.RawMessage, len(foundTxs))
	for i, tx := range foundTxs {
		jsonBytes, err := json.Marshal(tx)
		if err != nil {
			b.L.Error("could not marshal transaction", "blockHeight", tx.TxData.BlockHeight, "txIndex", tx.TxData.TxIndex)
			return false
		}
		jsonMessages[i] = json.RawMessage(jsonBytes)
	}
	// BOT-MAP-SKIP-1 (testnet 2026-10-06): a map that failed before landing (RC
	// pre-flight, network, node) used to be logged and the block skipped for good,
	// so that deposit was never credited. Keep the block and retry it next cycle.
	// A map the contract REFUSED on chain (ErrTxFailed) is final for this proof and
	// already recorded for /retry, so it does not hold the block. Re-sending a map
	// that did land is refused by the contract (the deposit is already observed).
	retryBlock := false
	for _, tx := range jsonMessages {
		if _, err := b.callWithRetry(ctx, tx, "map", broadcastRetryAttempts); err != nil {
			b.L.Error("map call failed", "err", err)
			if !errors.Is(err, ErrTxFailed) {
				retryBlock = true
			}
		}
	}
	if retryBlock {
		b.L.Warn("a map did not land for a transient reason; block kept for retry", "blockHeight", blockHeight)
		return false
	}

	advanced, err := b.stateDB().AdvanceBlockHeightIfCurrent(ctx, blockHeight, blockHeight+1)
	if err != nil {
		b.L.Error("error advancing last block height", "err", err, "blockHeight", blockHeight)
		return false
	}
	if !advanced {
		// Another bot instance likely advanced the height first.
		b.L.Info("block height already advanced by another instance", "blockHeight", blockHeight)
	}

	b.setLastBlock(blockHeight)
	return true
}

// HandleExistingTxs checks for existing txs for newly registered addresses.
// This is a best-effort scan — the main loop's block-by-block processing
// is the primary detection mechanism. The lookback window is chain-specific
// (b.Chain.HistoricalTxLookback), sized to roughly one week of chain time.
func (b *Bot) HandleExistingTxs(chainAddress string) {
	b.L.Debug("checking existing txs for new address", "address", chainAddress)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tipHeight, err := b.Chain.Client.GetTipHeight()
	if err != nil {
		b.L.Error("failed to fetch chain tip height for historical tx scan", "err", err)
		return
	}
	lookback := b.Chain.HistoricalTxLookback
	var minHeight uint64
	if tipHeight >= lookback {
		minHeight = tipHeight - lookback
	}

	entries, err := b.Chain.Client.GetAddressTxs(chainAddress)
	if err != nil {
		b.L.Error("failed to fetch address tx history", "address", chainAddress, "err", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	blockTxIDs := make(map[string]map[string]uint64)
	for _, entry := range entries {
		if !entry.Confirmed {
			continue
		}

		details, err := b.Chain.Client.GetTxDetails(entry.TxID)
		if err != nil {
			b.L.Warn("failed to fetch tx confirmation details", "txid", entry.TxID, "err", err)
			continue
		}
		if !details.Confirmed || details.BlockHash == "" {
			continue
		}
		if details.BlockHeight < minHeight {
			continue
		}
		if _, ok := blockTxIDs[details.BlockHash]; !ok {
			blockTxIDs[details.BlockHash] = make(map[string]uint64)
		}
		blockTxIDs[details.BlockHash][entry.TxID] = details.BlockHeight
	}

	for blockHash, wantedTxIDs := range blockTxIDs {
		var blockHeight uint64
		for _, h := range wantedTxIDs {
			blockHeight = h
			break
		}
		blockBytes, err := b.Chain.Client.GetRawBlock(blockHash)
		if err != nil {
			b.L.Warn("failed to fetch raw block for historical tx scan", "blockHash", blockHash, "err", err)
			continue
		}
		foundTxs, err := b.ParseBlock(ctx, blockBytes, blockHeight)
		if err != nil {
			b.L.Warn("failed to parse historical block", "blockHash", blockHash, "height", blockHeight, "err", err)
			continue
		}

		// Only map historical txs that were present in the address history.
		for _, tx := range foundTxs {
			txID := txIDFromRawTxHex(tx.TxData.RawTxHex)
			if _, ok := wantedTxIDs[txID]; !ok {
				continue
			}

			jsonBytes, err := json.Marshal(tx)
			if err != nil {
				b.L.Warn("could not marshal historical transaction", "err", err)
				continue
			}
			if _, err := b.callWithRetry(ctx, json.RawMessage(jsonBytes), "map", broadcastRetryAttempts); err != nil {
				b.L.Error("historical map call failed", "err", err)
			}
		}
	}
}

func txIDFromRawTxHex(rawTxHex string) string {
	rawTx, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return ""
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(rawTx)); err != nil {
		return ""
	}
	return tx.TxID()
}

// contractHeightNeededFor is the contract header height at which a deposit or
// spend mined in block h is deep enough for the contract to act on it.
//
// BOT-MAP-DEPTH-1 (testnet 2026-10-06): the bot mapped a block as soon as the
// contract held it (depth 0), the v2 contract refused the deposit as "not
// confirmed deeply enough yet", and the bot moved on to the next block anyway,
// so the deposit was never credited. Waiting for the depth makes the one attempt
// the bot makes per block land. Same rule for confirmSpend (BOT-CONF-1).
func (b *Bot) contractHeightNeededFor(h uint64) uint64 {
	if b.Chain == nil || b.Chain.ConfirmationsRequired <= 1 {
		return h
	}
	return h + b.Chain.ConfirmationsRequired - 1
}
