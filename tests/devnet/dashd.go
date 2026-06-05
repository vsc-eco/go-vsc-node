package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DashdRPCURL returns the URL the magi nodes use to talk to dashd from
// inside the docker network. Embeds the regtest user/pass declared in
// docker-compose.yml.
func (d *Devnet) DashdRPCURL() string {
	return "http://vsc-node-user:vsc-node-pass@dashd:19898"
}

// DashdRPCHostPort returns just the "host:port" part of the dashd RPC
// URL. This is what goes into the oracle's per-node Chains config
// (modules/oracle/config.go ChainRpcConfig.RpcHost).
func (d *Devnet) DashdRPCHostPort() string {
	return "dashd:19898"
}

// dashCli runs `dash-cli` inside the dashd container with the regtest
// credentials and returns the trimmed stdout.
func (d *Devnet) dashCli(ctx context.Context, args ...string) (string, error) {
	full := append([]string{
		"exec", "dashd",
		"dash-cli", "-regtest",
		"-rpcuser=vsc-node-user", "-rpcpassword=vsc-node-pass",
	}, args...)
	out, err := d.composeOutput(ctx, full...)
	return strings.TrimSpace(out), err
}

// MineDashBlocks mines `count` regtest blocks against a freshly generated
// regtest address. Returns the new tip height. Requires EnableDashd=true
// on the devnet config.
func (d *Devnet) MineDashBlocks(ctx context.Context, count int) (uint64, error) {
	if count < 1 {
		return 0, fmt.Errorf("count must be >= 1")
	}

	// Create wallet if missing. Ignore "already exists" error.
	_, _ = d.dashCli(ctx, "-named", "createwallet", "wallet_name=devnet", "load_on_startup=true")

	addr, err := d.dashCli(ctx, "getnewaddress")
	if err != nil {
		return 0, fmt.Errorf("getnewaddress: %w", err)
	}

	if _, err := d.dashCli(ctx, "generatetoaddress", fmt.Sprint(count), addr); err != nil {
		return 0, fmt.Errorf("generatetoaddress: %w", err)
	}

	return d.DashHeight(ctx)
}

// DashHeight returns the dashd regtest tip height.
func (d *Devnet) DashHeight(ctx context.Context) (uint64, error) {
	out, err := d.dashCli(ctx, "getblockcount")
	if err != nil {
		return 0, fmt.Errorf("getblockcount: %w", err)
	}
	var h uint64
	if _, err := fmt.Sscanf(out, "%d", &h); err != nil {
		return 0, fmt.Errorf("parsing block count %q: %w", out, err)
	}
	return h, nil
}

// GetDashBlockHeaderHex returns the 80-byte raw block header at `height` as
// a lowercase hex string (160 chars), suitable for passing to the Dash
// mapping contract's seedBlocks / addBlocks actions.
func (d *Devnet) GetDashBlockHeaderHex(ctx context.Context, height uint64) (string, error) {
	hash, err := d.dashCli(ctx, "getblockhash", fmt.Sprint(height))
	if err != nil {
		return "", fmt.Errorf("getblockhash %d: %w", height, err)
	}
	// getblockheader <hash> false → raw 80-byte hex (verbose=true returns JSON)
	hdr, err := d.dashCli(ctx, "getblockheader", hash, "false")
	if err != nil {
		return "", fmt.Errorf("getblockheader %s: %w", hash, err)
	}
	if len(hdr) != 160 {
		return "", fmt.Errorf("unexpected header hex length %d (want 160): %q", len(hdr), hdr)
	}
	return hdr, nil
}

// SendDashTo sends `amountDash` to `addr` from the dashd regtest wallet
// and returns the txid. `amountDash` is a decimal string (e.g. "0.001"
// for 100_000 duffs). The wallet must be loaded — usually
// MineDashBlocks(ctx, N) is called first to seed coins.
//
// Used by tests/devnet's IS-login E2E to fund the per-session deposit
// address after /session/start hands one back. With the IS service's
// -testBypassDashdISLock=true flag active, the dashd watcher fires
// onObserved as soon as the tx hits mempool (without waiting for the
// regtest-unavailable LLMQ IS-lock).
func (d *Devnet) SendDashTo(ctx context.Context, addr string, amountDash string) (string, error) {
	// Ensure the wallet exists + has a spendable balance. Mining N
	// blocks gives the wallet the coinbase reward but it needs
	// `COINBASE_MATURITY=100` confirmations on regtest. Caller is
	// responsible for mining enough headroom (~110 blocks before
	// expecting a spend to succeed).
	_, _ = d.dashCli(ctx, "-named", "createwallet", "wallet_name=devnet", "load_on_startup=true")
	txid, err := d.dashCli(ctx, "sendtoaddress", addr, amountDash)
	if err != nil {
		return "", fmt.Errorf("sendtoaddress %s %s: %w", addr, amountDash, err)
	}
	return txid, nil
}

// GetDashRawTransaction returns the raw transaction hex for `txid`
// via dashd's `getrawtransaction <txid> 0` (verbose=0 → just hex).
// Used by tests/devnet's IS-login E2E to fetch the same rawTxHex the
// IS-service watcher passed into onObserved, so the harness can build
// canonical-message hashes that match what the orchestrator computes.
func (d *Devnet) GetDashRawTransaction(ctx context.Context, txid string) (string, error) {
	out, err := d.dashCli(ctx, "getrawtransaction", txid, "0")
	if err != nil {
		return "", fmt.Errorf("getrawtransaction %s: %w", txid, err)
	}
	return out, nil
}

// ResolveDashTxSenderAddress returns the first input's spent-address for
// `txid`. Walks one hop back through the dashd verbose RPC:
//
//	1. getrawtransaction <txid> 1 → vin[0].txid + vin[0].vout
//	2. getrawtransaction <vin0.txid> 1 → vout[vin0.vout].scriptPubKey.address(es)
//
// Mirrors the mapping contract's resolveDashDIDFromTxInputs walk
// (forwarder_integration.go:254) but without requiring the test to link
// btcd's wire / chaincfg / txscript packages — dashd does the address
// decoding for us. Used to compute the DashDID the mapping contract
// will see for an op=call payment so test scaffolding can pre-fund
// that DID's contract-internal HBD via the regtest-only seedInternalHbd
// admin enabler.
//
// Single-input simplification: this returns the FIRST input's address.
// Spec §5.2.5 requires strict same-address for all inputs; the regtest
// wallet's sendtoaddress only ever uses single-address inputs for small
// amounts, so this matches the mapping contract's behaviour for the
// test fixture. If the dashd wallet ever picks multi-address inputs
// the mapping contract will reject the tx with a "multi-address inputs"
// ErrInput — the test will see ATTESTATION_TIMEOUT instead of advancing,
// which is the same shape as any other ATTESTATION failure.
func (d *Devnet) ResolveDashTxSenderAddress(ctx context.Context, txid string) (string, error) {
	out, err := d.dashCli(ctx, "getrawtransaction", txid, "1")
	if err != nil {
		return "", fmt.Errorf("getrawtransaction %s 1: %w", txid, err)
	}
	var spending struct {
		Vin []struct {
			Txid string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
	}
	if err := json.Unmarshal([]byte(out), &spending); err != nil {
		return "", fmt.Errorf("decode verbose tx %s: %w", txid, err)
	}
	if len(spending.Vin) == 0 {
		return "", fmt.Errorf("tx %s has no inputs", txid)
	}
	// Skip coinbase (no prev-tx; coinbase tx has empty Txid).
	if spending.Vin[0].Txid == "" {
		return "", fmt.Errorf("tx %s vin[0] is coinbase, no sender to resolve", txid)
	}
	prevOut, err := d.dashCli(ctx, "getrawtransaction", spending.Vin[0].Txid, "1")
	if err != nil {
		return "", fmt.Errorf("getrawtransaction (prev) %s: %w", spending.Vin[0].Txid, err)
	}
	var prev struct {
		Vout []struct {
			N            uint32 `json:"n"`
			ScriptPubKey struct {
				Address   string   `json:"address"`
				Addresses []string `json:"addresses"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	if err := json.Unmarshal([]byte(prevOut), &prev); err != nil {
		return "", fmt.Errorf("decode prev tx %s: %w", spending.Vin[0].Txid, err)
	}
	for _, vout := range prev.Vout {
		if vout.N == spending.Vin[0].Vout {
			// dashd's newer RPC uses singular "address"; older paths
			// return "addresses". Try both.
			if vout.ScriptPubKey.Address != "" {
				return vout.ScriptPubKey.Address, nil
			}
			if len(vout.ScriptPubKey.Addresses) > 0 {
				return vout.ScriptPubKey.Addresses[0], nil
			}
			return "", fmt.Errorf("prev tx %s vout %d has no decoded address",
				spending.Vin[0].Txid, spending.Vin[0].Vout)
		}
	}
	return "", fmt.Errorf("prev tx %s missing vout %d", spending.Vin[0].Txid, spending.Vin[0].Vout)
}

// dashRegtestGenesisCAIP2Hex is the bip122 chain-genesis hex slug used
// for regtest/testnet DashDIDs. Mirrors lib/dids/dash.go's
// DashTestnetDIDPrefix and dash-mapping-contract's
// dashGenesisCAIP2Hex (regtest/testnet branch).
const dashRegtestGenesisCAIP2Hex = "00000bafbc94add76cb75e2ec9289483"

// DashTestnetDIDFromAddress builds the testnet/regtest DashDID for
// the given Dash address. Format mirrors the mapping contract's
// ResolveSenderDashDID return shape so the seedInternalHbd payload
// can be addressed directly. Exposed for op=call test scaffolding.
func DashTestnetDIDFromAddress(addr string) string {
	return "did:pkh:bip122:" + dashRegtestGenesisCAIP2Hex + ":" + addr
}

// WaitForDashHeight blocks until the dashd regtest tip is at least `target`
// blocks tall, or the timeout expires.
func (d *Devnet) WaitForDashHeight(ctx context.Context, target uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		h, err := d.DashHeight(ctx)
		if err == nil && h >= target {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dashd never reached height %d (last err: %v)", target, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
