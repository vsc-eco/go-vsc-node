package mapper

import (
	"context"
	"encoding/json"

	"github.com/hasura/go-graphql-client"
)

const observedContractKey = "observed_txs"
const txSpendsContractKey = "tx_spends"

const contractId = ""

type GetContractStateQuery struct {
	GetStateByKeys map[string]string `graphql:"getStateByKeys(contractId: $contractId, keys: $keys)"`
}

func FetchContractData(client *graphql.Client) (map[string]bool, map[string]*SigningData, error) {
	var query GetContractStateQuery
	variables := map[string]any{
		"contractId": contractId,
		"keys":       []string{observedContractKey, txSpendsContractKey},
	}
	err := client.Query(context.TODO(), &query, variables)
	if err != nil {
		return nil, nil, err
	}

	var obserbedTxs map[string]bool
	observedTxsJson := query.GetStateByKeys[observedContractKey]
	err = json.Unmarshal([]byte(observedTxsJson), &obserbedTxs)
	if err != nil {
		return nil, nil, err
	}

	var txSpends map[string]*SigningData
	txSpendsJson := query.GetStateByKeys[txSpendsContractKey]
	err = json.Unmarshal([]byte(txSpendsJson), &txSpendsJson)
	if err != nil {
		return nil, nil, err
	}

	return obserbedTxs, txSpends, nil
}
