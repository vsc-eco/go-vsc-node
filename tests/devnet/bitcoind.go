package devnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// StartBitcoindPruned starts the bitcoind-pruned container (manual pruning
// mode, connected to the archive bitcoind as a P2P peer). Requires the
// archive bitcoind to be running and healthy first (EnableBitcoind=true).
func (d *Devnet) StartBitcoindPruned(ctx context.Context) error {
	if err := d.compose(ctx, "--profile", "bitcoind-pruned", "up", "-d", "bitcoind-pruned"); err != nil {
		return fmt.Errorf("starting bitcoind-pruned: %w", err)
	}
	if err := d.waitForService(ctx, "bitcoind-pruned", 2*time.Minute); err != nil {
		return fmt.Errorf("bitcoind-pruned health check: %w", err)
	}
	return nil
}

// bitcoinCliPruned runs bitcoin-cli inside the bitcoind-pruned container.
func (d *Devnet) bitcoinCliPruned(ctx context.Context, args ...string) (string, error) {
	full := append([]string{
		"exec", "bitcoind-pruned",
		"bitcoin-cli", "-regtest",
		"-rpcuser=vsc-node-user", "-rpcpassword=vsc-node-pass",
	}, args...)
	out, err := d.composeOutput(ctx, full...)
	return strings.TrimSpace(out), err
}

// BitcoinPrunedHeight returns the tip height of the pruned bitcoind.
func (d *Devnet) BitcoinPrunedHeight(ctx context.Context) (uint64, error) {
	out, err := d.bitcoinCliPruned(ctx, "getblockcount")
	if err != nil {
		return 0, fmt.Errorf("getblockcount (pruned): %w", err)
	}
	var h uint64
	if _, err := fmt.Sscanf(out, "%d", &h); err != nil {
		return 0, fmt.Errorf("parsing block count %q: %w", out, err)
	}
	return h, nil
}

// WaitForBitcoinPrunedSync waits until the pruned node's tip reaches target.
func (d *Devnet) WaitForBitcoinPrunedSync(ctx context.Context, target uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		h, err := d.BitcoinPrunedHeight(ctx)
		if err == nil && h >= target {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bitcoind-pruned never reached height %d (last err: %v)", target, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// PruneBlockchain calls pruneblockchain on the pruned node, removing block
// data up to (but not including) the given height. Returns the last pruned
// block height as reported by bitcoind. A return value of 0 means nothing
// was pruned (bitcoind returns 0 when the chain is too short).
func (d *Devnet) PruneBlockchain(ctx context.Context, height uint64) (uint64, error) {
	out, err := d.bitcoinCliPruned(ctx, "pruneblockchain", fmt.Sprint(height))
	if err != nil {
		return 0, fmt.Errorf("pruneblockchain %d: %w", height, err)
	}
	out = strings.TrimSpace(out)

	// bitcoin-cli returns a plain integer for the pruned height.
	// A value of 0 (or -1 in some versions) means nothing was pruned.
	var pruned int64
	if _, err := fmt.Sscanf(out, "%d", &pruned); err != nil {
		return 0, fmt.Errorf("parsing pruneblockchain output %q: %w", out, err)
	}
	if pruned <= 0 {
		return 0, fmt.Errorf("pruneblockchain returned %d — chain data likely below 550 MB minimum", pruned)
	}
	return uint64(pruned), nil
}

// BitcoindPrunedRPCHostPort returns the "host:port" for the pruned bitcoind
// RPC inside the docker network.
func (d *Devnet) BitcoindPrunedRPCHostPort() string {
	return "bitcoind-pruned:18443"
}

// ── Mainnet-pruned bitcoind helpers ──────────────────────────────────────

// StartBitcoindMainnetPruned starts a mainnet bitcoind with -prune=550 that
// connects to real Bitcoin peers via DNS seeds. Mainnet blocks are large
// enough that auto-pruning kicks in within a few minutes of IBD, making
// this a reliable way to test the pruned-block recovery path without
// needing to generate artificial block data.
func (d *Devnet) StartBitcoindMainnetPruned(ctx context.Context) error {
	if err := d.compose(ctx, "--profile", "bitcoind-mainnet-pruned", "up", "-d", "bitcoind-mainnet-pruned"); err != nil {
		return fmt.Errorf("starting bitcoind-mainnet-pruned: %w", err)
	}
	if err := d.waitForService(ctx, "bitcoind-mainnet-pruned", 2*time.Minute); err != nil {
		return fmt.Errorf("bitcoind-mainnet-pruned health check: %w", err)
	}
	return nil
}

// bitcoinCliMainnet runs bitcoin-cli inside the mainnet-pruned container.
// Note: no -regtest flag.
func (d *Devnet) bitcoinCliMainnet(ctx context.Context, args ...string) (string, error) {
	full := append([]string{
		"exec", "bitcoind-mainnet-pruned",
		"bitcoin-cli",
		"-rpcport=18443",
		"-rpcuser=vsc-node-user", "-rpcpassword=vsc-node-pass",
	}, args...)
	out, err := d.composeOutput(ctx, full...)
	return strings.TrimSpace(out), err
}

// BitcoindMainnetPrunedRPCHostPort returns the docker-internal RPC endpoint
// for the mainnet-pruned bitcoind.
func (d *Devnet) BitcoindMainnetPrunedRPCHostPort() string {
	return "bitcoind-mainnet-pruned:18443"
}

// WaitForMainnetPruning polls getblockchaininfo on the mainnet-pruned node
// until pruneheight > 0 (meaning auto-pruning has kicked in and early blocks
// are no longer available). Returns the prune height.
func (d *Devnet) WaitForMainnetPruning(ctx context.Context, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	for {
		out, err := d.bitcoinCliMainnet(ctx, "getblockchaininfo")
		if err == nil {
			var info struct {
				Blocks      uint64 `json:"blocks"`
				PruneHeight uint64 `json:"pruneheight"`
			}
			if jsonErr := json.Unmarshal([]byte(out), &info); jsonErr == nil && info.PruneHeight > 0 {
				return info.PruneHeight, nil
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("mainnet pruning never started within %v", timeout)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// ── Regtest block data fill ─────────────────────────────────────────────

// FillBlockData generates enough block data on the archive bitcoind to
// exceed 550 MB (the minimum before pruneblockchain will prune). It runs
// a shell loop inside the container to avoid per-RPC docker exec overhead,
// creating ~60 KB OP_RETURN transactions and mining them one per block.
//
// Requires -datacarriersize=400000 on the bitcoind.
func (d *Devnet) FillBlockData(ctx context.Context) error {
	// Ensure wallet exists and has mature coinbase outputs.
	d.bitcoinCli(ctx, "-named", "createwallet", "wallet_name=devnet", "load_on_startup=true")
	addr, err := d.bitcoinCli(ctx, "getnewaddress")
	if err != nil {
		return fmt.Errorf("getnewaddress: %w", err)
	}
	if _, err := d.bitcoinCli(ctx, "generatetoaddress", "110", addr); err != nil {
		return fmt.Errorf("mining initial blocks: %w", err)
	}

	// Generate a 60 KB hex payload for OP_RETURN transactions.
	opReturnData := hex.EncodeToString(make([]byte, 60_000))

	// Run the fill loop inside the container as a single shell command.
	// This avoids the per-iteration docker exec overhead (~50ms each)
	// that made the Go RPC approach too slow for ~10,000 iterations.
	//
	// The script:
	//   1. Creates an OP_RETURN tx, funds it, signs it, sends it, mines a block.
	//   2. Every 100 blocks, checks size_on_disk and logs progress.
	//   3. Exits when size_on_disk >= 600 MB.
	script := fmt.Sprintf(`#!/bin/bash
set -e
CLI="bitcoin-cli -regtest -rpcuser=vsc-node-user -rpcpassword=vsc-node-pass"
ADDR="%s"
DATA="%s"
TARGET=600000000
i=0

while true; do
    if [ $((i %% 100)) -eq 0 ]; then
        SIZE=$($CLI getblockchaininfo | sed -n 's/.*"size_on_disk": *\([0-9]*\).*/\1/p')
        echo "fill: $i blocks, $((SIZE / 1000000)) MB / 600 MB"
        if [ "$SIZE" -ge "$TARGET" ] 2>/dev/null; then
            echo "target reached"
            exit 0
        fi
    fi

    RAW=$($CLI createrawtransaction '[]' "{\"data\":\"$DATA\"}")
    FUNDED=$($CLI fundrawtransaction "$RAW" | sed -n 's/.*"hex": *"\([^"]*\)".*/\1/p')
    SIGNED=$($CLI signrawtransactionwithwallet "$FUNDED" | sed -n 's/.*"hex": *"\([^"]*\)".*/\1/p')
    $CLI sendrawtransaction "$SIGNED" > /dev/null
    $CLI generatetoaddress 1 "$ADDR" > /dev/null

    i=$((i + 1))
done
`, addr, opReturnData)

	log.Printf("[devnet] filling block data inside container (target: 600 MB)...")

	out, err := d.composeOutput(ctx,
		"exec", "-T", "bitcoind",
		"bash", "-c", script)
	if err != nil {
		return fmt.Errorf("block data fill script failed: %w\noutput: %s", err, out)
	}

	// Log the script's output (progress lines).
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			log.Printf("[devnet] %s", line)
		}
	}

	return nil
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
