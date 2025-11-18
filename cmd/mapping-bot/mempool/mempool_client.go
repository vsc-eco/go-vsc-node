package mempool

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

const MempoolAPIBase = "https://mempool.space/testnet/api"

type MempoolClient struct {
	baseURL string
	client  *http.Client
}

func NewMempoolClient() *MempoolClient {
	return &MempoolClient{
		baseURL: MempoolAPIBase,
		client:  &http.Client{},
	}
}

func (m *MempoolClient) GetBlockHashAtHeight(height uint32) (string, int, error) {
	fmt.Println("getting hash for block at height", height)
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
	fmt.Println("getting raw data for block with hash", hash)
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

func (m *MempoolClient) PostTx(rawTx string) error {
	url := fmt.Sprintf("%s/tx", m.baseURL)
	resp, err := m.client.Post(url, "test/plain", bytes.NewReader([]byte(rawTx)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
