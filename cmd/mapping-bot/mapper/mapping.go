package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"
)

// dropHeightDiff is set per-chain via ChainConfig.DropHeightDiff

func (b *Bot) HandleMap(
	blockBytes []byte,
	blockHeight uint64,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	lastContractHeightStr, err := b.gql().FetchLastHeight(ctx)
	if err != nil {
		log.Printf("error fetching contract's last block height: %s", err.Error())
		return
	}
	lastContractHeight, err := strconv.ParseUint(lastContractHeightStr, 10, 32)
	if err != nil {
		log.Printf("response recieved for last contract height is not an integer: %s", lastContractHeightStr)
		return
	}

	if lastContractHeight < blockHeight {
		log.Printf("delaying processing of block with height %d, block not yet present in contract", blockHeight)
		return
	}

	foundTxs, err := b.ParseBlock(ctx, blockBytes, blockHeight)
	if err != nil {
		log.Printf("error parsing block: %s", err.Error())
		return
	}

	jsonMessages := make([]json.RawMessage, len(foundTxs))
	for i, tx := range foundTxs {
		jsonBytes, err := json.Marshal(tx)
		if err != nil {
			fmt.Printf(
				"Could not marshall transaction in block at height %d with index %d.\n",
				tx.TxData.BlockHeight,
				tx.TxData.TxIndex,
			)
			return
		}
		jsonMessages[i] = json.RawMessage(jsonBytes)
	}
	for _, tx := range jsonMessages {
		if err := b.callWithRetry(ctx, tx, "map", 3); err != nil {
			log.Printf("map call failed after retries: %s", err.Error())
		}
	}

	_, err = b.stateDB().IncrementBlockHeight(ctx)
	if err != nil {
		log.Printf("error incrementing last block height: %s", err.Error())
		return
	}

	b.setLastBlock(blockHeight)
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
