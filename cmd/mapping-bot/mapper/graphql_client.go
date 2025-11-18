package mapper

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"

	"github.com/hasura/go-graphql-client"
)

const observedContractPrefix = "observed_txs"
const txSpendsContractKey = "tx_spends"

const contractId = "vsc1BVgE4NL3nZwtoDn82XMymNPriRUp9UVAGU"

type GetContractStateQuery struct {
	GetStateByKeys json.RawMessage `graphql:"getStateByKeys(contractId: $contractId, keys: $keys)"`
}

func (m *MapperState) FetchTxSpends() (map[string]*SigningData, error) {
	var query GetContractStateQuery

	variables := map[string]any{
		"contractId": contractId,
		"keys":       []string{txSpendsContractKey},
	}
	err := m.GqlClient.Query(context.TODO(), &query, variables, graphql.OperationName("GetContractState"))
	if err != nil {
		return nil, err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return nil, err
	}

	var txSpends map[string]*SigningData
	if txSpendsData, exists := stateMap[txSpendsContractKey]; exists && txSpendsData != nil {
		err = json.Unmarshal(txSpendsData, &txSpends)
		if err != nil {
			return nil, err
		}
	} else {
		txSpends = make(map[string]*SigningData)
	}

	return txSpends, nil
}

// TODO: use individual utxos (txid:vout) instead of just txids
func FetchObservedTx(client *graphql.Client, txId string, vout int) (bool, error) {
	var query GetContractStateQuery

	key := observedContractPrefix + fmt.Sprintf("%s:%d", txId, vout)

	variables := map[string]any{
		"contractId": contractId,
		"keys":       []string{key},
	}
	err := client.Query(context.TODO(), &query, variables, graphql.OperationName("GetContractState"))
	if err != nil {
		return false, err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return false, err
	}

	log.Println("statemap", stateMap)

	value := string(stateMap[key])
	exists := value != "null"
	return exists, nil
}

const keyId = ""

func FetchSignatures(client *graphql.Client, msgHex []string) (map[string][]byte, error) {
	var query struct {
		Tss []struct {
			Msg string `graphql:"msg"`
			Sig string `graphql:"sig"`
		} `graphql:"getTssRequests(keyId: $keyId, msgHex: $msgHex)"`
	}

	variables := map[string]any{
		"keyId":  keyId,
		"msgHex": msgHex,
	}

	opName := graphql.OperationName("GetTssRequests")
	if err := client.Query(context.Background(), &query, variables, opName); err != nil {
		return nil, fmt.Errorf("failed graphql query: %w", err)
	}

	out := make(map[string][]byte)
	for _, tss := range query.Tss {
		buf, err := hex.DecodeString(tss.Sig)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to decode signature [signature:%s]: %w",
				tss.Sig, err,
			)
		}
		out[tss.Msg] = buf
	}

	return nil, nil
}
