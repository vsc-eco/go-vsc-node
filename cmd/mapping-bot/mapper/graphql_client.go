package mapper

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"

	graphql "github.com/hasura/go-graphql-client"
)

type GetContractStateQuery struct {
	GetStateByKeys json.RawMessage `graphql:"getStateByKeys(contractId: $contractId, keys: $keys, encoding: $encoding)"`
}

func (b *Bot) fetchMultipleTxSpendKeys(
	ctx context.Context,
	registry []string,
) (map[string]*contractinterface.SigningData, error) {
	var query GetContractStateQuery

	keys := make([]string, len(registry))
	for i, txId := range registry {
		keys[i] = contractinterface.TxSpendsPrefix + txId
	}

	vars2 := map[string]any{
		"contractId": b.BotConfig.ContractId(),
		"keys":       keys,
		"encoding":   "hex",
	}

	err := b.GqlClient.Query(ctx, &query, vars2, graphql.OperationName("GetContractState"))
	if err != nil {
		return nil, err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return nil, err
	}

	var txSpends = make(map[string]*contractinterface.SigningData, len(registry))
	for i, txId := range registry {
		spendJson, ok := stateMap[keys[i]]
		if !ok {
			log.Printf("tx spend registry data does not match listed spends")
		} else {
			var tmp string
			err = json.Unmarshal(spendJson, &tmp)
			if err != nil {
				return nil, err
			}
			decoded, err := hex.DecodeString(tmp)
			if err != nil {
				return nil, fmt.Errorf("error decoding tx spend hex for tx id %s: %w", txId, err)
			}
			var spend contractinterface.SigningData
			if _, err := spend.UnmarshalMsg(decoded); err != nil {
				return nil, fmt.Errorf("error unmarshalling tx spend for tx id %s: %w", txId, err)
			}
			txSpends[txId] = &spend
		}
	}

	return txSpends, nil
}

// returns a map of transaction Ids to unsigned data that was submitted to be signed
func (b *Bot) FetchTxSpends(ctx context.Context) (map[string]*contractinterface.SigningData, error) {
	var query GetContractStateQuery

	vars1 := map[string]any{
		"contractId": b.BotConfig.ContractId(),
		"keys":       []string{contractinterface.TxSpendsRegistryKey},
		"encoding":   "hex",
	}
	err := b.GqlClient.Query(ctx, &query, vars1, graphql.OperationName("GetContractState"))
	if err != nil {
		return nil, err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return nil, err
	}

	var txSpendsRegistry contractinterface.TxSpendsRegistry
	if txSpendsData, exists := stateMap[contractinterface.TxSpendsRegistryKey]; exists &&
		string(txSpendsData) != `"null"` {
		var tmp string
		err = json.Unmarshal(txSpendsData, &tmp)
		if err != nil {
			return nil, err
		}
		decoded, err := hex.DecodeString(tmp)
		if err != nil {
			return nil, fmt.Errorf("error decoding tx spends registry hex: %w", err)
		}
		txSpendsRegistry, err = contractinterface.UnmarshalTxSpendsRegistry(decoded)
		if err != nil {
			return nil, err
		}
	}

	var txSpends map[string]*contractinterface.SigningData
	if len(txSpendsRegistry) > 0 {
		txSpends, err = b.fetchMultipleTxSpendKeys(ctx, txSpendsRegistry)
		if err != nil {
			return nil, err
		}
	} else {
		txSpends = make(map[string]*contractinterface.SigningData)
	}

	return txSpends, nil
}

// TODO: use individual utxos (txid:vout) instead of just txids
func (b *Bot) FetchObservedTx(ctx context.Context, txId string, vout int) (bool, error) {
	var query GetContractStateQuery

	key := contractinterface.ObservedPrefix + fmt.Sprintf("%s:%d", txId, vout)

	variables := map[string]any{
		"contractId": b.BotConfig.ContractId(),
		"keys":       []string{key},
		"encoding":   "hex",
	}
	err := b.GqlClient.Query(ctx, &query, variables, graphql.OperationName("GetContractState"))
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

func (b *Bot) FetchSignatures(
	ctx context.Context, msgHex []string,
) (map[string][]byte, error) {
	var query struct {
		Tss []struct {
			Msg    string `graphql:"msg"`
			Sig    string `graphql:"sig"`
			Status string `graphql:"status"`
		} `graphql:"getTssRequests(keyId: $keyId, msgHex: $msgHex)"`
	}

	variables := map[string]any{
		"keyId":  strings.Join([]string{b.BotConfig.ContractId(), "main"}, "-"),
		"msgHex": msgHex,
	}

	opName := graphql.OperationName("GetTssRequests")
	if err := b.GqlClient.Query(ctx, &query, variables, opName); err != nil {
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

// FetchPublicKeys fetches the primary and backup public keys from contract state.
// Keys are stored as raw bytes in the contract and returned as hex strings.
func (b *Bot) FetchPublicKeys(ctx context.Context) (primaryKeyHex []byte, backupKeyHex []byte, err error) {
	var query GetContractStateQuery

	variables := map[string]any{
		"contractId": b.BotConfig.ContractId(),
		"keys":       []string{contractinterface.PrimaryPublicKeyStateKey, contractinterface.BackupPublicKeyStateKey},
		"encoding":   "hex",
	}
	err = b.GqlClient.Query(ctx, &query, variables, graphql.OperationName("GetContractState"))
	if err != nil {
		return nil, nil, err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return nil, nil, err
	}

	primaryRaw, err := unmarshalRawBytes(stateMap[contractinterface.PrimaryPublicKeyStateKey])
	if err != nil || len(primaryRaw) == 0 {
		return nil, nil, fmt.Errorf("primary public key not found in contract state")
	}

	backupRaw, _ := unmarshalRawBytes(stateMap[contractinterface.BackupPublicKeyStateKey])

	return primaryRaw, backupRaw, nil
}

// unmarshalRawBytes decodes a JSON value that is a hex-encoded byte string.
// Returns nil for null/missing values.
func unmarshalRawBytes(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `"null"` {
		return nil, nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}

	return hex.DecodeString(s)
}

// gets last height recorded in contract state
func (b *Bot) FetchLastHeight(ctx context.Context) (string, error) {
	var query GetContractStateQuery

	variables := map[string]any{
		"contractId": b.BotConfig.ContractId(),
		"keys":       []string{contractinterface.LastHeightKey},
		"encoding":   "hex",
	}
	err := b.GqlClient.Query(ctx, &query, variables, graphql.OperationName("GetContractState"))
	if err != nil {
		return "", err
	}

	var stateMap map[string]json.RawMessage
	err = json.Unmarshal(query.GetStateByKeys, &stateMap)
	if err != nil {
		return "", err
	}

	var tmp string
	err = json.Unmarshal(stateMap[contractinterface.LastHeightKey], &tmp)
	if err != nil {
		return "", err
	}
	decoded, err := hex.DecodeString(tmp)
	if err != nil {
		return "", fmt.Errorf("error decoding last height hex: %w", err)
	}
	return string(decoded), nil
}
