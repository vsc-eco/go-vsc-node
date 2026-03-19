package mapper

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// dropHeightDiff is set per-chain via ChainConfig.DropHeightDiff

// HandleMap processes a single block for mapping transactions.
// Returns true if the block was processed, false if skipped (e.g., contract not ready).
func (b *Bot) HandleMap(
	blockBytes []byte,
	blockHeight uint64,
) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
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

	if lastContractHeight < blockHeight {
		b.L.Info("delaying processing, block not yet present in contract", "blockHeight", blockHeight, "contractHeight", lastContractHeight)
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
	for _, tx := range jsonMessages {
		if err := b.callWithRetry(ctx, tx, "map", 3); err != nil {
			b.L.Error("map call failed after retries", "err", err)
		}
	}

	_, err = b.stateDB().IncrementBlockHeight(ctx)
	if err != nil {
		b.L.Error("error incrementing last block height", "err", err)
		return false
	}

	b.setLastBlock(blockHeight)
	return true
}

// HandleExistingTxs checks for existing txs for newly registered addresses.
// This is a best-effort scan — the main loop's block-by-block processing
// is the primary detection mechanism.
func (b *Bot) HandleExistingTxs(chainAddress string) {
	b.L.Debug("checking existing txs for new address", "address", chainAddress)
	// Existing tx detection is handled by the main block processing loop.
	// This function is a placeholder for future address-history scanning
	// which requires chain-specific implementation.
}
