package devnet

import (
	"context"
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
