package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"
	"vsc-node/cmd/mapping-bot/mempool"
)

// height, in blocks, below the current height at which transactions should be dropped
const dropHeightDiff = 4320

func (b *Bot) HandleMap(
	blockBytes []byte,
	blockHeight uint64,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	lastContractHeightStr, err := b.FetchLastHeight(ctx)
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
		err := b.callContract(ctx, tx, "map")
		if err != nil {
			log.Printf("error calling contract: %s", err.Error())
		}
	}

	_, err = b.Db.State.IncrementBlockHeight(ctx)
	if err != nil {
		log.Printf("error incrementing last block height: %s", err.Error())
		return
	}

	b.setLastBlock(blockHeight)
}

func groupTxsByBlock(transactions []mempool.Transaction, lastHeight uint64) map[string][]mempool.Transaction {
	// rarely will have txs that are in the same block so allocate length to same as array
	grouped := make(map[string][]mempool.Transaction, len(transactions))

	for _, tx := range transactions {
		// don't acknowledge blocks older than the drop diff (and can break here because the rest will be older)
		if (lastHeight - tx.Status.BlockHeight) < dropHeightDiff {
			break
		}
		blockHash := tx.Status.BlockHash
		if !tx.Status.Confirmed {
			blockHash = "" // Group unconfirmed transactions together
		}
		grouped[blockHash] = append(grouped[blockHash], tx)
	}

	return grouped
}

// checks for existing txs for new addresses being registered
func (b *Bot) HandleExistingTxs(btcAddress string) {
	txHistory, err := b.MempoolClient.GetAddressTxs(btcAddress)
	if err != nil {
		log.Printf("failed to fetch transaction history for address %s: %s", btcAddress, err)
		return
	}
	if len(txHistory) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lastHeight, err := b.Db.State.GetBlockHeight(ctx)
	if err != nil {
		log.Printf("failed to retrieve last height from database")
		return
	}

	txMap := groupTxsByBlock(txHistory, lastHeight)

	for blockHash, txs := range txMap {
		blockHeight := txs[0].Status.BlockHeight
		blockBytes, err := b.MempoolClient.GetRawBlock(blockHash)
		if err != nil {
			log.Printf("error getting block with hash %s: %s", blockHash, err.Error())
		}
		b.HandleMap(blockBytes, blockHeight)
	}
}
