package mempool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

const MempoolMainnetAPIBase = "https://mempool.space/api"
const MempoolTestnet4APIBase = "https://mempool.space/testnet4/api"
const MempoolTestnet3APIBase = "https://mempool.space/testnet/api"

type MempoolClient struct {
	baseURL string
	client  *http.Client
}

// types for getting address tx history
type Transaction struct {
	TxID     string   `json:"txid"`
	Version  int      `json:"version"`
	Locktime int64    `json:"locktime"`
	Vin      []Input  `json:"vin"`
	Vout     []Output `json:"vout"`
	Size     int      `json:"size"`
	Weight   int      `json:"weight"`
	Sigops   int      `json:"sigops"`
	Fee      int64    `json:"fee"`
	Status   Status   `json:"status"`
}

type Input struct {
	TxID                  string   `json:"txid"`
	Vout                  int      `json:"vout"`
	Prevout               Prevout  `json:"prevout"`
	Scriptsig             string   `json:"scriptsig"`
	ScriptsigAsm          string   `json:"scriptsig_asm"`
	Witness               []string `json:"witness"`
	IsCoinbase            bool     `json:"is_coinbase"`
	Sequence              uint32   `json:"sequence"`
	InnerWitnessscriptAsm string   `json:"inner_witnessscript_asm,omitempty"`
}

type Prevout struct {
	Scriptpubkey        string `json:"scriptpubkey"`
	ScriptpubkeyAsm     string `json:"scriptpubkey_asm"`
	ScriptpubkeyType    string `json:"scriptpubkey_type"`
	ScriptpubkeyAddress string `json:"scriptpubkey_address"`
	Value               int64  `json:"value"`
}

type Output struct {
	Scriptpubkey        string `json:"scriptpubkey"`
	ScriptpubkeyAsm     string `json:"scriptpubkey_asm"`
	ScriptpubkeyType    string `json:"scriptpubkey_type"`
	ScriptpubkeyAddress string `json:"scriptpubkey_address"`
	Value               int64  `json:"value"`
}

type Status struct {
	Confirmed   bool   `json:"confirmed"`
	BlockHeight uint64 `json:"block_height"`
	BlockHash   string `json:"block_hash"`
	BlockTime   uint64 `json:"block_time"`
}

func NewMempoolClient(httpClient *http.Client, baseURL string) *MempoolClient {
	return &MempoolClient{
		baseURL: baseURL,
		client:  httpClient,
	}
}

func (m *MempoolClient) GetTipHeight() (uint64, error) {
	url := fmt.Sprintf("%s/blocks/tip/height", m.baseURL)
	resp, err := m.client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("mempool API returned status %d", resp.StatusCode)
	}

	var height uint64
	if err := json.NewDecoder(resp.Body).Decode(&height); err != nil {
		return 0, fmt.Errorf("error decoding tip height: %w", err)
	}
	return height, nil
}

func (m *MempoolClient) GetBlockHashAtHeight(height uint64) (string, int, error) {
	slog.Info("getting hash for block", "height", height)
	url := fmt.Sprintf("%s/block-height/%d", m.baseURL, height)
	resp, err := m.client.Get(url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, fmt.Errorf("mempool API returned status %d", resp.StatusCode)
	}

	blockHash, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, err
	}

	return string(blockHash), resp.StatusCode, nil
}

func (m *MempoolClient) GetRawBlock(hash string) ([]byte, error) {
	slog.Info("getting raw data for block", "hash", hash)
	url := fmt.Sprintf("%s/block/%s/raw", m.baseURL, hash)
	resp, err := m.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mempool API returned status %d", resp.StatusCode)
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return rawBytes, nil
}

func (m *MempoolClient) GetAddressTxs(btcAddress string) ([]Transaction, error) {
	url := fmt.Sprintf("%s/address/%s/txs/chain", m.baseURL, btcAddress)
	resp, err := m.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mempool API returned status %d", resp.StatusCode)
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var txHistory []Transaction
	err = json.Unmarshal(rawBytes, &txHistory)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling json response: %w", err)
	}

	return txHistory, nil
}

func (m *MempoolClient) PostTx(rawTx string) error {
	url := fmt.Sprintf("%s/tx", m.baseURL)
	resp, err := m.client.Post(url, "text/plain", bytes.NewReader([]byte(rawTx)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mempool API returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
