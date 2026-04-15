package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"

	graphql "github.com/hasura/go-graphql-client"
)

// ---------------------------------------------------------------------------
// Fallback helpers
// ---------------------------------------------------------------------------

// gqlHTTPPost sends bodyBytes as a JSON POST to each configured URL in order,
// stopping at the first URL that responds with a 2xx status. The decode func
// is called with the successful response and its result is returned directly
// (application-level errors such as GraphQL errors are NOT retried).
// Network errors and non-2xx HTTP responses trigger fallback to the next URL.
func (b *Bot) gqlHTTPPost(ctx context.Context, bodyBytes []byte, decode func(*http.Response) error) error {
	urls := b.gqlURLs
	if b.GqlURL != "" {
		urls = []string{b.GqlURL}
	}
	var lastErr error
	for _, url := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
		if err != nil {
			// Bad URL — non-retriable.
			return fmt.Errorf("build request for %s: %w", url, err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue // network error — try next URL
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
			continue // server error — try next URL
		}
		decErr := decode(resp)
		resp.Body.Close()
		return decErr // success or application error — don't retry
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("no GraphQL endpoints configured")
}

// gqlClientDo calls fn with each configured graphql.Client in order, stopping
// at the first success. Only errors from fn trigger fallback to the next client.
// fn should create a fresh query struct on each invocation to avoid partial-state
// issues across retries.
func (b *Bot) gqlClientDo(fn func(*graphql.Client) error) error {
	clients := b.gqlClients
	if b.GqlClient != nil {
		clients = []*graphql.Client{b.GqlClient}
	}
	var lastErr error
	for _, client := range clients {
		if err := fn(client); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	if len(clients) == 0 {
		return errors.New("no GraphQL clients configured")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Contract state queries (hasura graphql-client based)
// ---------------------------------------------------------------------------

type GetContractStateQuery struct {
	GetStateByKeys json.RawMessage `graphql:"getStateByKeys(contractId: $contractId, keys: $keys, encoding: $encoding)"`
}

func (b *Bot) fetchMultipleTxSpendKeys(
	ctx context.Context,
	registry []string,
) (map[string]*contractinterface.SigningData, error) {
	keys := make([]string, len(registry))
	for i, txId := range registry {
		keys[i] = contractinterface.TxSpendsPrefix + txId
	}

	vars := map[string]interface{}{
		"contractId": b.BotConfig.ContractId(),
		"keys":       keys,
		"encoding":   "hex",
	}

	var result json.RawMessage
	err := b.gqlClientDo(func(client *graphql.Client) error {
		var q GetContractStateQuery
		if err := client.Query(ctx, &q, vars, graphql.OperationName("GetContractState")); err != nil {
			return err
		}
		result = q.GetStateByKeys
		return nil
	})
	if err != nil {
		return nil, err
	}

	var stateMap map[string]json.RawMessage
	if err := json.Unmarshal(result, &stateMap); err != nil {
		return nil, err
	}

	txSpends := make(map[string]*contractinterface.SigningData, len(registry))
	for i, txId := range registry {
		spendJson, ok := stateMap[keys[i]]
		if !ok {
			log.Printf("tx spend registry data does not match listed spends")
		} else {
			var tmp string
			if err := json.Unmarshal(spendJson, &tmp); err != nil {
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
	vars := map[string]interface{}{
		"contractId": b.BotConfig.ContractId(),
		"keys":       []string{contractinterface.TxSpendsRegistryKey},
		"encoding":   "hex",
	}

	var result json.RawMessage
	err := b.gqlClientDo(func(client *graphql.Client) error {
		var q GetContractStateQuery
		if err := client.Query(ctx, &q, vars, graphql.OperationName("GetContractState")); err != nil {
			return err
		}
		result = q.GetStateByKeys
		return nil
	})
	if err != nil {
		return nil, err
	}

	var stateMap map[string]json.RawMessage
	if err := json.Unmarshal(result, &stateMap); err != nil {
		return nil, err
	}

	var txSpendsRegistry contractinterface.TxSpendsRegistry
	if txSpendsData, exists := stateMap[contractinterface.TxSpendsRegistryKey]; exists &&
		string(txSpendsData) != `"null"` {
		var tmp string
		if err := json.Unmarshal(txSpendsData, &tmp); err != nil {
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

	if len(txSpendsRegistry) > 0 {
		return b.fetchMultipleTxSpendKeys(ctx, txSpendsRegistry)
	}
	return make(map[string]*contractinterface.SigningData), nil
}

// TODO: use individual utxos (txid:vout) instead of just txids
func (b *Bot) FetchObservedTx(ctx context.Context, txId string, vout int) (bool, error) {
	key := contractinterface.ObservedPrefix + fmt.Sprintf("%s:%d", txId, vout)

	vars := map[string]interface{}{
		"contractId": b.BotConfig.ContractId(),
		"keys":       []string{key},
		"encoding":   "hex",
	}

	var result json.RawMessage
	err := b.gqlClientDo(func(client *graphql.Client) error {
		var q GetContractStateQuery
		if err := client.Query(ctx, &q, vars, graphql.OperationName("GetContractState")); err != nil {
			return err
		}
		result = q.GetStateByKeys
		return nil
	})
	if err != nil {
		return false, err
	}

	var stateMap map[string]json.RawMessage
	if err := json.Unmarshal(result, &stateMap); err != nil {
		return false, err
	}

	value := string(stateMap[key])
	exists := value != "null"
	return exists, nil
}

func (b *Bot) FetchSignatures(
	ctx context.Context, msgHex []string,
) (map[string]database.SignatureUpdate, error) {
	vars := map[string]interface{}{
		"keyId":  strings.Join([]string{b.BotConfig.ContractId(), "main"}, "-"),
		"msgHex": msgHex,
	}

	type tssRow struct {
		Msg    string `graphql:"msg"`
		Sig    string `graphql:"sig"`
		Status string `graphql:"status"`
	}
	var rows []tssRow

	err := b.gqlClientDo(func(client *graphql.Client) error {
		var q struct {
			Tss []tssRow `graphql:"getTssRequests(keyId: $keyId, msgHex: $msgHex)"`
		}
		if err := client.Query(ctx, &q, vars, graphql.OperationName("GetTssRequests")); err != nil {
			return err
		}
		rows = q.Tss
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed graphql query: %w", err)
	}

	out := make(map[string]database.SignatureUpdate)
	for _, tss := range rows {
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
		out[tss.Msg] = database.SignatureUpdate{Bytes: buf, IsBackup: false}
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// Raw HTTP GraphQL queries
// ---------------------------------------------------------------------------

// FetchTransactionStatus queries the VSC node for a transaction's current status.
func (b *Bot) FetchTransactionStatus(ctx context.Context, txId string) (string, error) {
	reqBody, err := json.Marshal(map[string]any{
		"query": `query FindTransaction($filterOptions: TransactionFilter) {
			findTransaction(filterOptions: $filterOptions) {
				status
			}
		}`,
		"variables": map[string]any{
			"filterOptions": map[string]any{
				"byId": txId,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal graphql request: %w", err)
	}

	var result struct {
		Data struct {
			FindTransaction []struct {
				Status string `json:"status"`
			} `json:"findTransaction"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	err = b.gqlHTTPPost(ctx, reqBody, func(resp *http.Response) error {
		return json.NewDecoder(resp.Body).Decode(&result)
	})
	if err != nil {
		return "", fmt.Errorf("failed to query transaction status: %w", err)
	}
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("graphql error: %s", result.Errors[0].Message)
	}
	if len(result.Data.FindTransaction) == 0 {
		return "", fmt.Errorf("transaction %s not found", txId)
	}
	return result.Data.FindTransaction[0].Status, nil
}

// FetchAccountNonce queries the VSC node for the next unused nonce of a given
// account (did:* or hive:*). Used as the nonce header on L2-submitted txs.
func (b *Bot) FetchAccountNonce(ctx context.Context, account string) (uint64, error) {
	reqBody, err := json.Marshal(map[string]any{
		"query": `query($a: String!){ getAccountNonce(account: $a){ nonce } }`,
		"variables": map[string]any{
			"a": account,
		},
	})
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	var result struct {
		Data struct {
			GetAccountNonce struct {
				Nonce uint64 `json:"nonce"`
			} `json:"getAccountNonce"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	err = b.gqlHTTPPost(ctx, reqBody, func(resp *http.Response) error {
		return json.NewDecoder(resp.Body).Decode(&result)
	})
	if err != nil {
		return 0, fmt.Errorf("fetch nonce: %w", err)
	}
	if len(result.Errors) > 0 {
		return 0, fmt.Errorf("graphql error: %s", result.Errors[0].Message)
	}
	return result.Data.GetAccountNonce.Nonce, nil
}

// SubmitTransactionV1 submits a signed VSC L2 transaction via the node's
// submitTransactionV1 mutation and returns the resulting CID tx ID.
func (b *Bot) SubmitTransactionV1(ctx context.Context, txB64, sigB64 string) (string, error) {
	reqBody, err := json.Marshal(map[string]any{
		"query": `query($tx: String!, $sig: String!){ submitTransactionV1(tx: $tx, sig: $sig){ id } }`,
		"variables": map[string]any{
			"tx":  txB64,
			"sig": sigB64,
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	var result struct {
		Data struct {
			SubmitTransactionV1 struct {
				ID *string `json:"id"`
			} `json:"submitTransactionV1"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	err = b.gqlHTTPPost(ctx, reqBody, func(resp *http.Response) error {
		return json.NewDecoder(resp.Body).Decode(&result)
	})
	if err != nil {
		return "", fmt.Errorf("submit tx: %w", err)
	}
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("graphql error: %s", result.Errors[0].Message)
	}
	if result.Data.SubmitTransactionV1.ID == nil {
		return "", fmt.Errorf("submitTransactionV1 returned nil id")
	}
	return *result.Data.SubmitTransactionV1.ID, nil
}

// FetchPublicKeys fetches the primary and backup public keys from contract state.
// Keys are stored as raw bytes in the contract and returned as hex strings.
func (b *Bot) FetchPublicKeys(ctx context.Context) (primaryKeyHex []byte, backupKeyHex []byte, err error) {
	vars := map[string]interface{}{
		"contractId": b.BotConfig.ContractId(),
		"keys":       []string{contractinterface.PrimaryPublicKeyStateKey, contractinterface.BackupPublicKeyStateKey},
		"encoding":   "hex",
	}

	var result json.RawMessage
	err = b.gqlClientDo(func(client *graphql.Client) error {
		var q GetContractStateQuery
		if err := client.Query(ctx, &q, vars, graphql.OperationName("GetContractState")); err != nil {
			return err
		}
		result = q.GetStateByKeys
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	var stateMap map[string]json.RawMessage
	if err = json.Unmarshal(result, &stateMap); err != nil {
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
	vars := map[string]interface{}{
		"contractId": b.BotConfig.ContractId(),
		"keys":       []string{contractinterface.LastHeightKey},
		"encoding":   "hex",
	}

	var result json.RawMessage
	err := b.gqlClientDo(func(client *graphql.Client) error {
		var q GetContractStateQuery
		if err := client.Query(ctx, &q, vars, graphql.OperationName("GetContractState")); err != nil {
			return err
		}
		result = q.GetStateByKeys
		return nil
	})
	if err != nil {
		return "", err
	}

	var stateMap map[string]json.RawMessage
	if err := json.Unmarshal(result, &stateMap); err != nil {
		return "", err
	}

	var tmp string
	if err := json.Unmarshal(stateMap[contractinterface.LastHeightKey], &tmp); err != nil {
		return "", err
	}
	decoded, err := hex.DecodeString(tmp)
	if err != nil {
		return "", fmt.Errorf("error decoding last height hex: %w", err)
	}
	return string(decoded), nil
}
