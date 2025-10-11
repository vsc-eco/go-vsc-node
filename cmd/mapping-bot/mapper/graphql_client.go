package mapper

import (
	"context"
	"encoding/json"

	"github.com/hasura/go-graphql-client"
)

const observedContractKey = "observed_txs"
const txSpendsContractKey = "tx_spends"

const contractId = "vsc1BonkE2CtHqjnkFdH8hoAEMP25bbWhSr3UA"

type GetContractStateQuery struct {
	GetStateByKeys json.RawMessage `graphql:"getStateByKeys(contractId: $contractId, keys: $keys)"`
}

func FetchContractData(client *graphql.Client) (map[string]bool, map[string]*SigningData, error) {
	var query GetContractStateQuery
	variables := map[string]any{
		"contractId": contractId,
		"keys":       []string{observedContractKey, txSpendsContractKey},
	}
	err := client.Query(context.TODO(), &query, variables, graphql.OperationName("GetContractState"))
	if err != nil {
		return nil, nil, err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return nil, nil, err
	}

	var observedTxs map[string]bool
	if observedData, exists := stateMap[observedContractKey]; exists && observedData != nil {
		observedTxsJson, _ := json.Marshal(observedData)
		err = json.Unmarshal(observedTxsJson, &observedTxs)
		if err != nil {
			return nil, nil, err
		}
	} else {
		observedTxs = make(map[string]bool)
	}

	var txSpends map[string]*SigningData
	if txSpendsData, exists := stateMap[txSpendsContractKey]; exists && txSpendsData != nil {
		txSpendsJson, _ := json.Marshal(txSpendsData)
		err = json.Unmarshal(txSpendsJson, &txSpends)
		if err != nil {
			return nil, nil, err
		}
	} else {
		txSpends = make(map[string]*SigningData)
	}

	return observedTxs, txSpends, nil
}

func FetchSignatures([]string) map[string][]byte {
	return nil
}
