// Package btcclient is a minimal, dependency-free esplora / mempool.space
// REST client for reading a BTC address's confirmed on-chain balance.
//
// It lives under lib/ (not cmd/mapping-bot/chain) so it can be imported from
// modules/ — the existing MempoolSpaceClient sits under a `main` package tree
// and cannot cross that import boundary, and it exposes no per-address balance
// method anyway (only GetAddressTxs). This client is used by the BTC TSS
// keysign solvency SIGNAL (modules/tss/solvency_gate.go) to compare the vault's
// real L1 holdings against the contract-claimed supply.
package btcclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client queries an esplora-compatible REST API (mempool.space, Blockstream,
// or a self-hosted esplora instance). The base URL is injected from config.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client for baseURL (e.g. "https://mempool.space/api"). A zero
// timeout falls back to 10s so a hung endpoint can never block the caller.
func New(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: timeout}}
}

// addressStats mirrors the esplora GET /address/{a} chain_stats object. The
// confirmed balance is funded_txo_sum - spent_txo_sum.
type addressStats struct {
	ChainStats struct {
		FundedTxoSum int64 `json:"funded_txo_sum"`
		SpentTxoSum  int64 `json:"spent_txo_sum"`
	} `json:"chain_stats"`
}

// AddressBalanceSats returns the confirmed on-chain balance (in satoshis) of a
// single address via esplora GET /address/{address}. Context-aware so the
// caller can bound the whole probe.
func (c *Client) AddressBalanceSats(ctx context.Context, address string) (int64, error) {
	url := fmt.Sprintf("%s/address/%s", c.baseURL, address)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("esplora returned status %d for address %s", resp.StatusCode, address)
	}
	var stats addressStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return 0, fmt.Errorf("decoding address stats: %w", err)
	}
	return stats.ChainStats.FundedTxoSum - stats.ChainStats.SpentTxoSum, nil
}
