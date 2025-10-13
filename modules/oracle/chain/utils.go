package chain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"vsc-node/modules/db/vsc/contracts"
	transactionpool "vsc-node/modules/transaction-pool"

	"github.com/hasura/go-graphql-client"
	blocks "github.com/ipfs/go-block-format"
)

const baseOracleDid = "did:vsc:oracle:"

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
		Caller:     baseOracleDid + symbol,
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

func makeChainSessionID(
	symbol string,
	startBlock, endBlock uint64,
) (string, error) {
	if endBlock <= startBlock {
		return "", errors.New("invalid bound")
	}

	id := fmt.Sprintf("%s-%d-%d", symbol, startBlock, endBlock)

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

func getAccountNonce(symbol string) (uint64, error) {
	client := graphql.NewClient("https://api.vsc.eco/api/v1/graphql", nil)

	username := baseOracleDid + strings.ToLower(symbol)
	var query struct {
		Data struct {
			Nonce uint64 `graphql:"nonce"`
		} `graphql:"getAccountNonce(account: $account)"`
	}

	variables := map[string]any{
		"account": username,
	}

	opName := graphql.OperationName("GetAccountNonce")
	if err := client.Query(context.Background(), &query, variables, opName); err != nil {
		return 0, fmt.Errorf("failed graphql query: %w", err)
	}

	return query.Data.Nonce, nil
}

func makeChainTx(
	chain chainRelay,
	chainData []chainBlock,
) (blocks.Block, error) {
	var err error
	payload := make([]string, len(chainData))
	for i, block := range chainData {
		payload[i], err = block.Serialize()
		if err != nil {
			return nil, fmt.Errorf(
				"failed to serialize block %d: %w",
				block.BlockHeight(), err,
			)
		}
	}

	nonce, err := getAccountNonce(chain.Symbol())
	if err != nil {
		return nil, fmt.Errorf("failed to get nonce value: %w", err)
	}

	tx, err := makeSignableBlock(
		chain.ContractID(),
		chain.Symbol(),
		payload,
		nonce,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to make transaction: %w", err)
	}

	return tx, nil
}

func (c *ChainOracle) makeChainSession(
	symbol string,
) (chainSession, error) {
	symbol = strings.ToLower(symbol)

	chain, ok := c.chainRelayers[symbol]
	if !ok {
		return chainSession{}, errInvalidChainSymbol
	}

	latestChainState, err := chain.GetLatestValidHeight()
	if err != nil {
		return chainSession{}, fmt.Errorf(
			"failed to get latest chain height: %s",
			err,
		)
	}

	contractState, err := chain.GetContractState()
	if err != nil {
		return chainSession{}, fmt.Errorf(
			"failed to get vsc contract state: %s",
			err,
		)
	}

	hasNewBlock := latestChainState.blockHeight > contractState.blockHeight
	if !hasNewBlock {
		return chainSession{}, nil
	}

	const heightLimit = 100
	startBlock := contractState.blockHeight + 1
	endBlock := min(startBlock+heightLimit, startBlock+heightLimit)

	sessionID, err := makeChainSessionID(symbol, startBlock, endBlock)
	if err != nil {
		return chainSession{}, fmt.Errorf("failed to make sessionID: %w", err)
	}

	_, chainData, err := getSessionData(c.chainRelayers, sessionID)
	if err != nil {
		return chainSession{}, fmt.Errorf("failed to get chainData: %w", err)
	}

	session := chainSession{
		sessionID:         sessionID,
		symbol:            chain.Symbol(),
		contractId:        chain.ContractID(),
		chainData:         chainData,
		newBlocksToSubmit: hasNewBlock,
	}

	return session, nil
}

func getSessionData(
	chainMap map[string]chainRelay,
	sessionID string,
) (chainRelay, []chainBlock, error) {
	symbol, startBlock, endBlock, err := parseChainSessionID(sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"failed to parse chainSessionID [%s]: %w",
			sessionID,
			err,
		)
	}

	chain, ok := chainMap[strings.ToLower(symbol)]
	if !ok {
		return nil, nil, fmt.Errorf("invalid symbol %s", symbol)
	}

	count := (endBlock - startBlock) + 1
	chainData, err := chain.ChainData(startBlock, count)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get chainData: %w", err)
	}

	return chain, chainData, nil
}
