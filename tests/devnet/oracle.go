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

// SetTrustedForwarders updates the SysConfigOverrides JSON file in-place
// to set the TrustedForwarders list — contract IDs that may invoke the
// WASM `call_as` host function (i.e. set effectiveCaller to an arbitrary
// DID). Required for the dash-forwarder-contract to call into op=call
// target contracts on behalf of the Dash payer's DashDID.
//
// Magi nodes only re-read the sysconfig file at startup, so callers must
// restart the nodes for the change to take effect. Same flow as
// SetOracleContractIDs:
//
//	forwarderId, _ := d.DeployContract(ctx, ...)
//	_ = d.SetTrustedForwarders([]string{forwarderId})
//	_ = d.RestartAllMagiNodes(ctx)
//
// Without this, sdk.ContractCallAs from the forwarder aborts with
// `errMsg="call_as: caller contract:<id> is not in system-config.
// TrustedForwarders"` (see system-config.go's TrustedForwarders comment
// for the ERC-2771 analogy).
func (d *Devnet) SetTrustedForwarders(contractIds []string) error {
	if d.cfg.SysConfigOverrides == nil {
		d.cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{}
	}
	// Copy so callers' slice isn't aliased into devnet config.
	cp := make([]string, len(contractIds))
	copy(cp, contractIds)
	d.cfg.SysConfigOverrides.TrustedForwarders = &cp
	return writeSysConfigOverrides(d.cfg, d.devnetDir)
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

// RestartAllMagiNodes recreates every magi-N container so they re-read
// their config files. Useful after SetOracleContractIDs /
// SetTrustedForwarders.
//
// Uses `docker compose up -d --force-recreate` rather than stop+start
// because `start` after `stop` keeps the SAME container instance, and
// in some cases the container's PID 1 may not fully re-exec — i.e. the
// magid process keeps running with its initially-loaded sysconfig.
// `up --force-recreate` destroys + recreates the container, guaranteeing
// a fresh process that re-runs magid main() and calls LoadOverrides()
// against the current sysconfig.json on disk.
func (d *Devnet) RestartAllMagiNodes(ctx context.Context) error {
	names := make([]string, d.cfg.Nodes)
	for i := range names {
		names[i] = fmt.Sprintf("magi-%d", i+1)
	}

	log.Printf("[devnet] recreating all magi nodes to pick up config changes...")
	upArgs := append([]string{"up", "-d", "--force-recreate"}, names...)
	if err := d.compose(ctx, upArgs...); err != nil {
		return fmt.Errorf("recreating magi nodes: %w", err)
	}

	// Give nodes a moment to reconnect to peers + re-form gossipsub mesh.
	time.Sleep(15 * time.Second)
	return nil
}
