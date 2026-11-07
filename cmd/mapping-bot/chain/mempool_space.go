package chain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// MempoolSpaceClient implements BlockchainClient using the mempool.space REST API.
// Works for any chain that mempool.space supports (BTC mainnet/testnet, LTC via
// a custom instance, etc.).
type MempoolSpaceClient struct {
	baseURL string
	client  *http.Client
}

func NewMempoolSpaceClient(httpClient *http.Client, baseURL string) *MempoolSpaceClient {
	return &MempoolSpaceClient{baseURL: baseURL, client: httpClient}
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
	url := fmt.Sprintf("%s/blocks/tip/height", m.baseURL)
	resp, err := m.client.Get(url)
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
	url := fmt.Sprintf("%s/block-height/%d", m.baseURL, height)
	resp, err := m.client.Get(url)
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
	url := fmt.Sprintf("%s/block/%s/raw", m.baseURL, hash)
	resp, err := m.client.Get(url)
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
	url := fmt.Sprintf("%s/address/%s/txs/chain", m.baseURL, address)
	resp, err := m.client.Get(url)
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
	url := fmt.Sprintf("%s/tx/%s/status", m.baseURL, txid)
	resp, err := m.client.Get(url)
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
	txidsURL := fmt.Sprintf("%s/block/%s/txids", m.baseURL, status.BlockHash)
	txidsResp, err := m.client.Get(txidsURL)
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
	url := fmt.Sprintf("%s/tx", m.baseURL)
	resp, err := m.client.Post(url, "text/plain", bytes.NewReader([]byte(rawTx)))
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
