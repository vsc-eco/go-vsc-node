package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

// historicalTxLookback is the maximum number of blocks back that HandleExistingTxs
// will scan when a new address is registered. Transactions confirmed before this
// window are ignored.
const historicalTxLookback = 1080

// HandleExistingTxs checks for existing txs for newly registered addresses.
// This is a best-effort scan — the main loop's block-by-block processing
// is the primary detection mechanism.
func (b *Bot) HandleExistingTxs(chainAddress string) {
	b.L.Debug("checking existing txs for new address", "address", chainAddress)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tipHeight, err := b.Chain.Client.GetTipHeight()
	if err != nil {
		b.L.Error("failed to fetch chain tip height for historical tx scan", "err", err)
		return
	}
	var minHeight uint64
	if tipHeight >= historicalTxLookback {
		minHeight = tipHeight - historicalTxLookback
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
			if err := b.callWithRetry(ctx, json.RawMessage(jsonBytes), "map", 3); err != nil {
				b.L.Error("historical map call failed after retries", "err", err)
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
