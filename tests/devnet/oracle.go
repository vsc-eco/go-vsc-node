package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// oracleConfigJSON mirrors modules/oracle/config.go oracleConfig.
// Duplicated here so the devnet package doesn't need to import the oracle
// package (which pulls in heavy dependencies).
type oracleConfigJSON struct {
	Chains map[string]chainRpcConfigJSON `json:"Chains"`
}

type chainRpcConfigJSON struct {
	RpcHost string `json:"RpcHost"`
	RpcUser string `json:"RpcUser"`
	RpcPass string `json:"RpcPass"`
}

// WriteOracleConfigs writes a per-node oracleConfig.json into each magi
// node's data directory pointing enabled chains at their in-network regtest
// containers. Must be called after devnet-setup has created the data-${i}
// directories (i.e. after Devnet.Start has finished).
//
// This only sets the RPC connection details. Per-chain contract IDs are
// set separately via SetOracleContractIDs because they're only known
// after the contract has been deployed at runtime.
//
// At least one of EnableBitcoind or EnableDashd must be true.
func (d *Devnet) WriteOracleConfigs(ctx context.Context) error {
	if !d.cfg.EnableBitcoind && !d.cfg.EnableDashd {
		return fmt.Errorf("WriteOracleConfigs requires EnableBitcoind or EnableDashd")
	}

	chains := map[string]chainRpcConfigJSON{}
	if d.cfg.EnableBitcoind {
		chains["BTC"] = chainRpcConfigJSON{
			RpcHost: d.BitcoindRPCHostPort(),
			RpcUser: "vsc-node-user",
			RpcPass: "vsc-node-pass",
		}
	}
	if d.cfg.EnableDashd {
		chains["DASH"] = chainRpcConfigJSON{
			RpcHost: d.DashdRPCHostPort(),
			RpcUser: "vsc-node-user",
			RpcPass: "vsc-node-pass",
		}
	}

	cfg := oracleConfigJSON{Chains: chains}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling oracle config: %w", err)
	}

	for i := 1; i <= d.cfg.Nodes; i++ {
		nodeDir := filepath.Join(d.devnetDir, fmt.Sprintf("data-%d", i), "config")
		if err := os.MkdirAll(nodeDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", nodeDir, err)
		}
		path := filepath.Join(nodeDir, "oracleConfig.json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		targets := make([]string, 0, len(chains))
		for name, c := range chains {
			targets = append(targets, name+"="+c.RpcHost)
		}
		log.Printf("[devnet] wrote oracle config for magi-%d -> %v", i, targets)
	}
	return nil
}

// WriteOracleConfigsForHost is like WriteOracleConfigs but points the BTC
// chain at a specific RPC host (e.g. "bitcoind-pruned:18443") instead of
// the default archive bitcoind.
func (d *Devnet) WriteOracleConfigsForHost(ctx context.Context, rpcHost string) error {
	cfg := oracleConfigJSON{
		Chains: map[string]chainRpcConfigJSON{
			"BTC": {
				RpcHost: rpcHost,
				RpcUser: "vsc-node-user",
				RpcPass: "vsc-node-pass",
			},
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling oracle config: %w", err)
	}

	for i := 1; i <= d.cfg.Nodes; i++ {
		nodeDir := filepath.Join(d.devnetDir, fmt.Sprintf("data-%d", i), "config")
		if err := os.MkdirAll(nodeDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", nodeDir, err)
		}
		path := filepath.Join(nodeDir, "oracleConfig.json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		log.Printf("[devnet] wrote oracle config for magi-%d -> %s", i, rpcHost)
	}
	return nil
}

// SetOracleContractIDs updates the SysConfigOverrides JSON file in-place
// to set OracleParams.ChainContracts to the provided map. Must be called
// while the devnet is running. Magi nodes only re-read the sysconfig file
// at startup, so callers must restart the nodes (or a subset) for the
// change to take effect.
//
// Use case:
//
//	contractId, _ := d.DeployContract(ctx, ...)
//	_ = d.SetOracleContractIDs(map[string]string{"BTC": contractId})
//	_ = d.RestartAllMagiNodes(ctx)
func (d *Devnet) SetOracleContractIDs(contractIds map[string]string) error {
	if d.cfg.SysConfigOverrides == nil {
		d.cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{}
	}
	if d.cfg.SysConfigOverrides.OracleParams == nil {
		d.cfg.SysConfigOverrides.OracleParams = &params.OracleParams{}
	}
	d.cfg.SysConfigOverrides.OracleParams.ChainContracts = contractIds

	// Re-marshal the entire override file so it stays internally consistent.
	return writeSysConfigOverrides(d.cfg, d.devnetDir)
}

// RestartAllMagiNodes stops every magi-N container then starts them again
// so they re-read their config files. Useful after SetOracleContractIDs.
func (d *Devnet) RestartAllMagiNodes(ctx context.Context) error {
	names := make([]string, d.cfg.Nodes)
	for i := range names {
		names[i] = fmt.Sprintf("magi-%d", i+1)
	}

	log.Printf("[devnet] restarting all magi nodes to pick up config changes...")
	if err := d.compose(ctx, append([]string{"stop"}, names...)...); err != nil {
		return fmt.Errorf("stopping magi nodes: %w", err)
	}
	if err := d.compose(ctx, append([]string{"start"}, names...)...); err != nil {
		return fmt.Errorf("starting magi nodes: %w", err)
	}

	// Give nodes a moment to reconnect to peers.
	time.Sleep(8 * time.Second)
	return nil
}
