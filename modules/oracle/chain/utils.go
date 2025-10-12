package chain

import (
	"errors"
	"fmt"
	"strings"
	"vsc-node/modules/db/vsc/contracts"
	transactionpool "vsc-node/modules/transaction-pool"

	blocks "github.com/ipfs/go-block-format"
)

func makeSignableBlock(
	symbol string,
	contractID string,
	payload []string,
	nonce uint64,
) (blocks.Block, error) {
	op := transactionpool.VscContractCall{
		ContractId: contractID,
		Action:     "add_blocks",
		Payload:    strings.Join(payload, ""),
		Intents:    []contracts.Intent{},
		RcLimit:    1000,
		Caller:     "did:vsc:oracle:" + symbol,
		NetId:      "vsc-mainnet",
	}

	vOp, err := op.SerializeVSC()
	if err != nil {
		return nil, fmt.Errorf("failed to serialize vsc operation: %w", err)
	}

	vscTx := &transactionpool.VSCTransaction{
		Ops:   []transactionpool.VSCTransactionOp{vOp},
		Nonce: nonce,
		NetId: "vsc-mainnet",
	}

	signableBlock, err := vscTx.ToSignableBlock()
	if err != nil {
		return nil, fmt.Errorf("failed to make signable block: %w", err)
	}

	return signableBlock, nil
}

func makeChainSessionID(c *chainSession) (string, error) {
	if len(c.chainData) == 0 {
		return "", errors.New("chainData not supplied")
	}

	startBlock := c.chainData[0].BlockHeight()
	endBlock := c.chainData[len(c.chainData)-1].BlockHeight()

	id := fmt.Sprintf("%s-%d-%d", c.symbol, startBlock, endBlock)
	return id, nil
}

// symbol - startBlock - endBlock
func parseChainSessionID(sessionID string) (string, uint64, uint64, error) {
	var (
		chainSymbol string
		startBlock  uint64
		endBlock    uint64
	)

	_, err := fmt.Sscanf(
		sessionID,
		"%s-%d-%d",
		&chainSymbol,
		&startBlock,
		&endBlock,
	)
	if err != nil {
		return "", 0, 0, err
	}

	return chainSymbol, startBlock, endBlock, nil
}
