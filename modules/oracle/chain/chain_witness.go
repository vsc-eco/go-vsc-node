package chain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// witnessChainData independently verifies chain data and returns a BLS signature.
// The witness fetches the same blocks from its own RPC, builds the identical
// transaction, and signs the resulting CID with its BLS key.
func witnessChainData(c *ChainOracle, msg *chainOracleMessage) (*chainRelayResponse, error) {
	// Parse the session ID to get symbol, start, end blocks
	chainSymbol, _, startBlock, endBlock, err := parseChainSessionID(msg.SessionID)
	if err != nil {
		return nil, fmt.Errorf("invalid session id: %w", err)
	}

	// Parse the request payload
	var request chainRelayRequest
	if err := json.Unmarshal(msg.Payload, &request); err != nil {
		return nil, fmt.Errorf("failed to parse relay request: %w", err)
	}

	// Look up the local chain relayer for this symbol
	chain, ok := c.chainRelayers[strings.ToUpper(chainSymbol)]
	if !ok {
		return nil, errInvalidChainSymbol
	}

	// Verify the contract ID matches our local config
	localContractId := chain.ContractId()
	if localContractId == "" {
		return nil, fmt.Errorf("no contract ID configured locally for %s", chainSymbol)
	}
	if localContractId != request.ContractId {
		return nil, fmt.Errorf(
			"contract ID mismatch for %s: local=%s, request=%s",
			chainSymbol, localContractId, request.ContractId,
		)
	}

	// Independently fetch the same blocks from our own RPC
	count := (endBlock - startBlock) + 1
	chainDataStart := time.Now()
	blocks, err := chain.ChainData(startBlock, count)
	c.logger.Debug("witness chain data fetch",
		"symbol", chainSymbol,
		"blocks", count,
		"duration", time.Since(chainDataStart),
		"err", err,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch chain data for verification: %w", err)
	}

	if len(blocks) == 0 {
		return nil, fmt.Errorf("no blocks returned for %s %d-%d", chainSymbol, startBlock, endBlock)
	}

	// Build the exact same transaction payload the producer built
	payload, err := makeTransactionPayload(blocks)
	if err != nil {
		return nil, fmt.Errorf("failed to build transaction payload: %w", err)
	}

	payloadJson, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	tx := makeTransaction(request.ContractId, string(payloadJson), chainSymbol, request.NetId)

	// Hash the transaction to get the CID (must match what the producer computed)
	signableBlock, err := tx.ToSignableBlock()
	if err != nil {
		return nil, fmt.Errorf("failed to create signable block: %w", err)
	}

	txCid := signableBlock.Cid()

	// Sign the CID with our BLS key
	blsProvider, err := c.conf.BlsProvider()
	if err != nil {
		return nil, fmt.Errorf("failed to get BLS provider: %w", err)
	}

	sig, err := blsProvider.Sign(txCid)
	if err != nil {
		return nil, fmt.Errorf("failed to sign chain data: %w", err)
	}

	blsDid, err := c.conf.BlsDID()
	if err != nil {
		return nil, fmt.Errorf("failed to get BLS DID: %w", err)
	}

	c.logger.Debug("signed chain relay data",
		"symbol", chainSymbol,
		"blocks", fmt.Sprintf("%d-%d", startBlock, endBlock),
		"cid", txCid.String(),
	)

	return &chainRelayResponse{
		Signature: sig,
		Account:   c.conf.Get().HiveUsername,
		BlsDid:    blsDid.String(),
	}, nil
}
