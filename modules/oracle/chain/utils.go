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

	"encoding/json"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/vsc-eco/hivego"
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

func callContract(
	symbol string,
	contractID string,
	contractInput json.RawMessage,
	action string,
) (json.RawMessage, error) {
	username := baseOracleDid + symbol
	tx := stateEngine.TxVscCallContract{
		NetId:      "vsc-mainnet",
		Caller:     username,
		ContractId: contractID,
		Action:     action,
		Payload:    contractInput,
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	}

	txData := tx.ToData()
	txJson, err := json.Marshal(&txData)
	if err != nil {
		return nil, err
	}

	deployOp := hivego.CustomJsonOperation{
		RequiredAuths:        []string{username},
		RequiredPostingAuths: []string{username},
		Id:                   contractID,
		Json:                 string(txJson),
	}

	deployOpJsonBytes, err := json.Marshal(&deployOp)
	if err != nil {
		return nil, err
	}

	return json.RawMessage(deployOpJsonBytes), nil
}

func getAccountNonce(symbol string) (uint64, error) {
	client := graphql.NewClient("https://api.vsc.eco/api/v1/graphql", nil)

	username := baseOracleDid + symbol
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
