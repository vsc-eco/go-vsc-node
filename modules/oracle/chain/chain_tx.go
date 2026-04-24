package chain

import (
	"fmt"

	blocks "github.com/ipfs/go-block-format"
)

func buildChainBlock(chain chainRelay, chainData []chainBlock) (blocks.Block, error) {
	if builder, ok := chain.(payloadBuilder); ok {
		payload, err := builder.buildAddBlocksPayload(chainData)
		if err != nil {
			return nil, fmt.Errorf("failed to build chain payload: %w", err)
		}
		nonce, err := getAccountNonce(chain.Symbol())
		if err != nil {
			return nil, fmt.Errorf("failed to get nonce: %w", err)
		}
		return makeSignableBlock(chain.ContractID(), chain.Symbol(), payload, nonce)
	}
	return makeChainTx(chain, chainData)
}
