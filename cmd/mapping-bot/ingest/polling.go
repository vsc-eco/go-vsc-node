package ingest

import (
	"context"
	"encoding/json"
	"vsc-node/cmd/mapping-bot/mapper"

	"github.com/hasura/go-graphql-client"
)

const obserbedKey = "observed_txs"
const txSpendsKey = "tx_spends"

const contractId = ""

type GetContractStateQuery struct {
	GetStateByKeys map[string]string `graphql:"getStateByKeys(contractId: $contractId, keys: $keys)"`
}

func FetchLatest(client *graphql.Client) (map[string]bool, map[string]*mapper.SigningData, error) {
	var query GetContractStateQuery
	variables := map[string]any{
		"contractId": contractId,
		"keys":       []string{obserbedKey, txSpendsKey},
	}
	err := client.Query(context.TODO(), &query, variables)
	if err != nil {
		return nil, nil, err
	}

	var obserbedTxs map[string]bool
	observedTxsJson := query.GetStateByKeys[obserbedKey]
	err = json.Unmarshal([]byte(observedTxsJson), &obserbedTxs)
	if err != nil {
		return nil, nil, err
	}

	var txSpends map[string]*mapper.SigningData
	txSpendsJson := query.GetStateByKeys[txSpendsKey]
	err = json.Unmarshal([]byte(txSpendsJson), &txSpendsJson)
	if err != nil {
		return nil, nil, err
	}

	return obserbedTxs, txSpends, nil
}
