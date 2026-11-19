package chain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
)

// MempoolSpaceClient implements BlockchainClient using the mempool.space REST API.
// Works for any chain that mempool.space supports (BTC mainnet/testnet, LTC via
// a custom instance, etc.).
//
// B7 (EVM bot parity, B14 RPC failover; rollout B4: mempool.space rate-limited
// and then banned the testnet bot's IP on 10-17, and the single source stalled
// deposits and withdrawals alike): the client takes several Esplora-compatible
// base URLs. A request goes to the endpoint that answered last; one that does
// not answer (network error, HTTP 5xx or 429) fails over to the next. Any
// other answer, 404 included, stands, as the EVM bot fails over only when an
// endpoint does not answer.
type MempoolSpaceClient struct {
	baseURLs []string
	start    atomic.Int64
	client   *http.Client
}

func NewMempoolSpaceClient(httpClient *http.Client, baseURL string) *MempoolSpaceClient {
	return &MempoolSpaceClient{baseURLs: []string{baseURL}, client: httpClient}
}

// SetBaseURLs replaces the endpoint list (the -chain-api flag). Empty entries
// are dropped; an empty list keeps the current one.
func (m *MempoolSpaceClient) SetBaseURLs(urls []string) {
	var out []string
	for _, u := range urls {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			out = append(out, u)
		}
	}
	if len(out) > 0 {
		m.baseURLs = out
		m.start.Store(0)
	}
}

// BaseURLs returns the endpoints in use.
func (m *MempoolSpaceClient) BaseURLs() []string { return append([]string(nil), m.baseURLs...) }

// do sends the request built for each endpoint in turn, starting at the one
// that answered last, until one answers.
func (m *MempoolSpaceClient) do(build func(base string) (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	first := int(m.start.Load())
	n := len(m.baseURLs)
	for k := 0; k < n; k++ {
		i := (first + k) % n
		req, err := build(m.baseURLs[i])
		if err != nil {
			return nil, err
		}
		resp, err := m.client.Do(req)
		if err == nil && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			if i != first {
				m.start.Store(int64(i))
			}
			return resp, nil
		}
		if err == nil {
			err = fmt.Errorf("API returned status %d", resp.StatusCode)
			resp.Body.Close()
		}
		lastErr = err
		if k+1 < n {
			slog.Warn("B7: chain API endpoint did not answer, trying the next", "endpoint", i, "err", err)
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no chain API endpoint configured")
	}
	return nil, lastErr
}

func (m *MempoolSpaceClient) get(path string) (*http.Response, error) {
	return m.do(func(base string) (*http.Request, error) { return http.NewRequest(http.MethodGet, base+path, nil) })
}

// mempool.space API response types

type mempoolTxStatus struct {
	Confirmed   bool   `json:"confirmed"`
	BlockHeight uint64 `json:"block_height"`
	BlockHash   string `json:"block_hash"`
	BlockTime   uint64 `json:"block_time"`
}

type mempoolTx struct {
	TxID   string          `json:"txid"`
	Vout   []mempoolOutput `json:"vout"`
	Status mempoolTxStatus `json:"status"`
}

type mempoolOutput struct {
	ScriptpubkeyAddress string `json:"scriptpubkey_address"`
	Value               int64  `json:"value"`
}

// BlockchainClient implementation

func (m *MempoolSpaceClient) GetTipHeight() (uint64, error) {
	resp, err := m.get("/blocks/tip/height")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("API returned status %d", resp.StatusCode)
	}
	var height uint64
	if err := json.NewDecoder(resp.Body).Decode(&height); err != nil {
		return 0, fmt.Errorf("error decoding tip height: %w", err)
	}
	return height, nil
}

func (m *MempoolSpaceClient) GetBlockHashAtHeight(height uint64) (string, int, error) {
	slog.Info("getting hash for block", "height", height)
	resp, err := m.get(fmt.Sprintf("/block-height/%d", height))
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, fmt.Errorf("API returned status %d", resp.StatusCode)
	}
	blockHash, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(blockHash), resp.StatusCode, nil
}

func (m *MempoolSpaceClient) GetRawBlock(hash string) ([]byte, error) {
	slog.Info("getting raw data for block", "hash", hash)
	resp, err := m.get(fmt.Sprintf("/block/%s/raw", hash))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (m *MempoolSpaceClient) GetAddressTxs(address string) ([]TxHistoryEntry, error) {
	resp, err := m.get(fmt.Sprintf("/address/%s/txs/chain", address))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}
	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var txs []mempoolTx
	if err := json.Unmarshal(rawBytes, &txs); err != nil {
		return nil, fmt.Errorf("error unmarshalling response: %w", err)
	}

	entries := make([]TxHistoryEntry, len(txs))
	for i, tx := range txs {
		outputs := make([]TxOutput, len(tx.Vout))
		for j, out := range tx.Vout {
			outputs[j] = TxOutput{
				Address: out.ScriptpubkeyAddress,
				Value:   out.Value,
				Index:   uint32(j),
			}
		}
		entries[i] = TxHistoryEntry{
			TxID:      tx.TxID,
			Confirmed: tx.Status.Confirmed,
			Outputs:   outputs,
		}
	}
	return entries, nil
}

func (m *MempoolSpaceClient) GetTxStatus(txid string) (bool, error) {
	details, err := m.GetTxDetails(txid)
	if err != nil {
		return false, err
	}
	return details.Confirmed, nil
}

func (m *MempoolSpaceClient) GetTxDetails(txid string) (TxConfirmationDetails, error) {
	resp, err := m.get(fmt.Sprintf("/tx/%s/status", txid))
	if err != nil {
		return TxConfirmationDetails{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return TxConfirmationDetails{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return TxConfirmationDetails{}, fmt.Errorf("API returned status %d for tx %s", resp.StatusCode, txid)
	}
	var status mempoolTxStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return TxConfirmationDetails{}, fmt.Errorf("error decoding tx status: %w", err)
	}
	if !status.Confirmed {
		return TxConfirmationDetails{}, nil
	}

	// Fetch the ordered txid list for this block to find the tx's position.
	txidsResp, err := m.get(fmt.Sprintf("/block/%s/txids", status.BlockHash))
	if err != nil {
		return TxConfirmationDetails{}, fmt.Errorf("failed to fetch block txids: %w", err)
	}
	defer txidsResp.Body.Close()
	if txidsResp.StatusCode != http.StatusOK {
		return TxConfirmationDetails{}, fmt.Errorf("block txids API returned status %d", txidsResp.StatusCode)
	}
	var txids []string
	if err := json.NewDecoder(txidsResp.Body).Decode(&txids); err != nil {
		return TxConfirmationDetails{}, fmt.Errorf("error decoding block txids: %w", err)
	}
	txIndex := uint32(0)
	for i, id := range txids {
		if id == txid {
			txIndex = uint32(i)
			break
		}
	}

	return TxConfirmationDetails{
		Confirmed:   true,
		BlockHeight: status.BlockHeight,
		BlockHash:   status.BlockHash,
		TxIndex:     txIndex,
	}, nil
}

func (m *MempoolSpaceClient) PostTx(rawTx string) error {
	resp, err := m.do(func(base string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, base+"/tx", bytes.NewReader([]byte(rawTx)))
		if err == nil {
			req.Header.Set("Content-Type", "text/plain")
		}
		return req, err
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
