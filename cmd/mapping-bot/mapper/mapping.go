package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
	"vsc-node/cmd/mapping-bot/mempool"

	"github.com/btcsuite/btcd/chaincfg"
)

// height, in blocks, below the current height at which transactions should be dropped
const dropHeightDiff = 4320

func (ms *MapperState) HandleMap(
	blockBytes []byte,
	blockHeight uint32,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	lastContractHeightStr, err := FetchLastHeight(ctx, ms.GqlClient)
	if err != nil {
		log.Printf("error fetching contract's last block height: %s", err.Error())
		return
	}
	lastContractHeight, err := strconv.ParseUint(lastContractHeightStr, 10, 32)
	if err != nil {
		log.Printf("response recieved for last contract height is not an integer")
		return
	}

	if uint32(lastContractHeight) < blockHeight {
		log.Printf("delaying processing of block with height %d, block not yet present in contract", blockHeight)
		return
	}

	blockParser := NewBlockParser(ms.Db.Addresses, &chaincfg.TestNet3Params)

	foundTxs, err := blockParser.ParseBlock(ctx, ms.GqlClient, blockBytes, blockHeight)
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
		// TODO: input username and contract ID
		err := callContract("milo-hpr", "vsc1BVgE4NL3nZwtoDn82XMymNPriRUp9UVAGU", tx, "map")
		if err != nil {
			log.Printf("error calling contract: %s", err.Error())
		}
	}

	ms.Mutex.Lock()
	defer ms.Mutex.Unlock()
	ms.LastBlockHeight++
	lastBlockBytes := []byte(strconv.FormatUint(uint64(ms.LastBlockHeight), 10))
	ms.FfsDatastore.Put(ctx, lastBlockKey, lastBlockBytes)
}

func groupTxsByBlock(transactions []mempool.Transaction, lastHeight uint32) map[string][]mempool.Transaction {
	// rarely will have txs that are in the same block so allocate length to same as array
	grouped := make(map[string][]mempool.Transaction, len(transactions))

	for _, tx := range transactions {
		// don't acknowledge blocks older than the drop diff (and can break here because the rest will be older)
		if (lastHeight - uint32(tx.Status.BlockHeight)) < dropHeightDiff {
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
func (ms *MapperState) HandleExistingTxs(btcAddress string) {
	mempoolClient := mempool.NewMempoolClient()
	txHistory, err := mempoolClient.GetAddressTxs(btcAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to fetch transaction history for address %s: %s", btcAddress, err)
		return
	}
	if len(txHistory) == 0 {
		return
	}
	txMap := groupTxsByBlock(txHistory, ms.LastBlockHeight)

	for blockHash, txs := range txMap {
		blockHeight := txs[0].Status.BlockHeight
		blockBytes, err := mempoolClient.GetRawBlock(blockHash)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error getting block with hash %s: %s", blockHash, err.Error())
		}
		ms.HandleMap(blockBytes, uint32(blockHeight))
	}
}
