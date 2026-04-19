package chain

import (
	"encoding/json"
	"fmt"
	"strings"
)

type payloadBuilder interface {
	buildAddBlocksPayload(chainData []chainBlock) (string, error)
}

type EthAddBlocksPayload struct {
	Blocks    []EthBlockEntry `json:"blocks"`
	LatestFee uint64          `json:"latest_fee"`
}

type EthBlockEntry struct {
	BlockNumber      uint64 `json:"block_number"`
	TransactionsRoot string `json:"transactions_root"`
	ReceiptsRoot     string `json:"receipts_root"`
	BaseFeePerGas    uint64 `json:"base_fee_per_gas"`
	GasLimit         uint64 `json:"gas_limit"`
	Timestamp        uint64 `json:"timestamp"`
}

func (e *Ethereum) buildAddBlocksPayload(chainData []chainBlock) (string, error) {
	if len(chainData) == 0 {
		return "", fmt.Errorf("empty chain data")
	}
	entries := make([]EthBlockEntry, len(chainData))
	for i, block := range chainData {
		ethBlock, ok := block.(*ethereumBlock)
		if !ok {
			return "", fmt.Errorf("block %d is not an ethereumBlock", i)
		}
		entries[i] = EthBlockEntry{
			BlockNumber:      parseHexUint(ethBlock.Number),
			TransactionsRoot: strings.TrimPrefix(ethBlock.TransactionsRoot, "0x"),
			ReceiptsRoot:     strings.TrimPrefix(ethBlock.ReceiptsRoot, "0x"),
			BaseFeePerGas:    parseHexUint(ethBlock.BaseFeePerGas),
			GasLimit:         parseHexUint(ethBlock.GasLimit),
			Timestamp:        parseHexUint(ethBlock.Timestamp),
		}
	}
	latestBaseFee := entries[len(entries)-1].BaseFeePerGas
	payload := EthAddBlocksPayload{Blocks: entries, LatestFee: latestBaseFee}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}
	return string(bytes), nil
}

func parseHexUint(s string) uint64 {
	s = strings.TrimPrefix(s, "0x")
	var v uint64
	fmt.Sscanf(s, "%x", &v)
	return v
}
