package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BitcoindRPCURL returns the URL the magi nodes use to talk to bitcoind from
// inside the docker network. The host port is not exposed to the host by
// default — tests interact with bitcoind via docker exec helpers below.
//
// The URL embeds the regtest user/pass declared in docker-compose.yml.
func (d *Devnet) BitcoindRPCURL() string {
	return "http://vsc-node-user:vsc-node-pass@bitcoind:18443"
}

// BitcoindRPCHostPort returns just the "host:port" part of the bitcoind RPC
// URL. This is what gets put into the oracle's per-node Chains config
// (see modules/oracle/config.go ChainRpcConfig.RpcHost).
func (d *Devnet) BitcoindRPCHostPort() string {
	return "bitcoind:18443"
}

// bitcoinCli runs `bitcoin-cli` inside the bitcoind container with the
// regtest credentials and returns the trimmed stdout.
func (d *Devnet) bitcoinCli(ctx context.Context, args ...string) (string, error) {
	full := append([]string{
		"exec", "bitcoind",
		"bitcoin-cli", "-regtest",
		"-rpcuser=vsc-node-user", "-rpcpassword=vsc-node-pass",
	}, args...)
	out, err := d.composeOutput(ctx, full...)
	return strings.TrimSpace(out), err
}

// MineBlocks mines `count` regtest blocks against a freshly generated
// bech32 regtest address. Returns the new tip height.
//
// Requires EnableBitcoind=true on the devnet config.
func (d *Devnet) MineBlocks(ctx context.Context, count int) (uint64, error) {
	if count < 1 {
		return 0, fmt.Errorf("count must be >= 1")
	}

	// `bitcoin-cli -regtest -named getnewaddress address_type=bech32` returns
	// a fresh address we can mine to. The wallet is auto-created on first
	// use in regtest.
	if _, err := d.bitcoinCli(ctx, "-named", "createwallet", "wallet_name=devnet", "load_on_startup=true"); err != nil {
		// Likely "Wallet file already exists" — ignore and continue.
	}

	addr, err := d.bitcoinCli(ctx, "getnewaddress")
	if err != nil {
		return 0, fmt.Errorf("getnewaddress: %w", err)
	}

	if _, err := d.bitcoinCli(ctx, "generatetoaddress", fmt.Sprint(count), addr); err != nil {
		return 0, fmt.Errorf("generatetoaddress: %w", err)
	}

	return d.BitcoinHeight(ctx)
}

// BitcoinHeight returns the bitcoind regtest tip height.
func (d *Devnet) BitcoinHeight(ctx context.Context) (uint64, error) {
	out, err := d.bitcoinCli(ctx, "getblockcount")
	if err != nil {
		return 0, fmt.Errorf("getblockcount: %w", err)
	}
	var h uint64
	if _, err := fmt.Sscanf(out, "%d", &h); err != nil {
		return 0, fmt.Errorf("parsing block count %q: %w", out, err)
	}
	return h, nil
}

// WaitForBitcoinHeight blocks until the bitcoind regtest tip is at least
// `target` blocks tall, or the timeout expires.
func (d *Devnet) WaitForBitcoinHeight(ctx context.Context, target uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		h, err := d.BitcoinHeight(ctx)
		if err == nil && h >= target {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bitcoind never reached height %d (last err: %v)", target, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// pingBitcoindHTTP is a sanity helper for use during test development —
// it issues a single getblockchaininfo via the host network, assuming the
// bitcoind RPC port is exposed (which it isn't by default). Kept here for
// convenience but not currently called from tests.
func pingBitcoindHTTP(ctx context.Context, url string) error {
	body := bytes.NewBufferString(`{"jsonrpc":"1.0","id":"ping","method":"getblockchaininfo","params":[]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(out))
	}
	var parsed struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if parsed.Error != nil {
		return fmt.Errorf("rpc error: %v", parsed.Error)
	}
	return nil
}
