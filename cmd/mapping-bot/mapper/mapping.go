package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"vsc-node/cmd/mapping-bot/parser"

	"github.com/btcsuite/btcd/chaincfg"
)

func (ms *MapperState) HandleMap(blockBytes []byte, blockHeight uint32) {

	// map of vsc to btc addresses
	// addressRegistry := make(map[string]string)
	addressRegistry := map[string]string{
		"hive:milo-hpr": "tb1q9gxwgzzxs7d597nh8843tndtwl9qrdup02tc0xcltrlt2tjyg7xqhat2zx",
	}

	blockParser := parser.NewBlockParser(addressRegistry, &chaincfg.MainNetParams)

	foundTxs, err := blockParser.ParseBlock(blockBytes, blockHeight, ms.ObservedTxs)
	if err != nil {
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
		callContract("username", "contract_id", tx, "map")
		fmt.Printf("===\n\n%s\n\n===\n", string(tx))
	}

	ms.Mutex.Lock()
	defer ms.Mutex.Unlock()
	ms.LastBlockHeight++
	lastBlockBytes := []byte(strconv.FormatUint(uint64(ms.LastBlockHeight), 10))
	ms.FfsDatastore.Put(context.TODO(), lastBlockKey, lastBlockBytes)
}
