package mapper

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/hasura/go-graphql-client"
)

const observedContractPrefix = "observed_txs"
const txSpendRegistryContractKey = "tx_spend_registry"
const txSpendContractPrefix = "tx_spend"
const lastHeightContractKey = "last_block_height"

// new contract
const contractId = "vsc1BTpUPXMyvc6LNe38w5UNCNAURZHH6esBic"

// old contract
// const contractId = "vsc1BVgE4NL3nZwtoDn82XMymNPriRUp9UVAGU"

type GetContractStateQuery struct {
	GetStateByKeys json.RawMessage `graphql:"getStateByKeys(contractId: $contractId, keys: $keys)"`
}

func fetchMultipleTxSpendKeys(
	ctx context.Context,
	client *graphql.Client,
	registry []string,
) (map[string]*SigningData, error) {
	var query GetContractStateQuery

	keys := make([]string, len(registry))
	for i, txId := range registry {
		keys[i] = txSpendContractPrefix + txId
	}

	vars2 := map[string]any{
		"contractId": contractId,
		"keys":       keys,
	}

	err := client.Query(ctx, &query, vars2, graphql.OperationName("GetContractState"))
	if err != nil {
		return nil, err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return nil, err
	}

	var txSpends = make(map[string]*SigningData, len(registry))
	for i, txId := range registry {
		spendJson, ok := stateMap[keys[i]]
		if !ok {
			log.Printf("tx spend registry data does not match listed spends")
		} else {
			var spend SigningData
			var tmp string
			err = json.Unmarshal(spendJson, &tmp)
			if err != nil {
				return nil, err
			}
			err := json.Unmarshal([]byte(tmp), &spend)
			if err != nil {
				return nil, fmt.Errorf("error unmarshalling tx spend for tx id %s: %w", txId, err)
			}
			txSpends[txId] = &spend
		}
	}

	return txSpends, nil
}

// returns a map of transaction Ids to unsigned data that was submitted to be signed
func FetchTxSpends(ctx context.Context, client *graphql.Client) (map[string]*SigningData, error) {
	var query GetContractStateQuery

	vars1 := map[string]any{
		"contractId": contractId,
		"keys":       []string{txSpendRegistryContractKey},
	}
	err := client.Query(ctx, &query, vars1, graphql.OperationName("GetContractState"))
	if err != nil {
		return nil, err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return nil, err
	}

	var txSpendsRegistry []string
	if txSpendsData, exists := stateMap[txSpendRegistryContractKey]; exists && string(txSpendsData) != `"null"` {
		var tmp string
		err = json.Unmarshal(txSpendsData, &tmp)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal([]byte(tmp), &txSpendsRegistry)
		if err != nil {
			return nil, err
		}
	}

	var txSpends map[string]*SigningData
	if len(txSpendsRegistry) > 0 {
		txSpends, err = fetchMultipleTxSpendKeys(ctx, client, txSpendsRegistry)
		if err != nil {
			return nil, err
		}
	} else {
		txSpends = make(map[string]*SigningData)
	}

	return txSpends, nil
}

// TODO: use individual utxos (txid:vout) instead of just txids
func FetchObservedTx(ctx context.Context, client *graphql.Client, txId string, vout int) (bool, error) {
	var query GetContractStateQuery

	key := observedContractPrefix + fmt.Sprintf("%s:%d", txId, vout)

	variables := map[string]any{
		"contractId": contractId,
		"keys":       []string{key},
	}
	err := client.Query(ctx, &query, variables, graphql.OperationName("GetContractState"))
	if err != nil {
		return false, err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return false, err
	}

	value := string(stateMap[key])
	exists := value != "null"
	return exists, nil
}

func FetchSignatures(ctx context.Context, client *graphql.Client, msgHex []string) (map[string][]byte, error) {
	var query struct {
		Tss []struct {
			Msg    string `graphql:"msg"`
			Sig    string `graphql:"sig"`
			Status string `graphql:"status"`
		} `graphql:"getTssRequests(keyId: $keyId, msgHex: $msgHex)"`
	}

	variables := map[string]any{
		"keyId":  strings.Join([]string{contractId, "main"}, "-"),
		"msgHex": msgHex,
	}

	opName := graphql.OperationName("GetTssRequests")
	if err := client.Query(ctx, &query, variables, opName); err != nil {
		return nil, fmt.Errorf("failed graphql query: %w", err)
	}

	out := make(map[string][]byte)
	for _, tss := range query.Tss {
		if tss.Status != "complete" {
			continue
		}
		buf, err := hex.DecodeString(tss.Sig)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to decode signature [signature:%s]: %w",
				tss.Sig, err,
			)
		}
		out[tss.Msg] = buf
	}

	return out, nil
}

// gets last height recorded in contract state
func FetchLastHeight(ctx context.Context, client *graphql.Client) (string, error) {
	var query GetContractStateQuery

	variables := map[string]any{
		"contractId": contractId,
		"keys":       []string{lastHeightContractKey},
	}
	err := client.Query(ctx, &query, variables, graphql.OperationName("GetContractState"))
	if err != nil {
		return "", err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return "", err
	}

	value := strings.ReplaceAll(string(stateMap[lastHeightContractKey]), "\"", "")
	return value, nil
}
