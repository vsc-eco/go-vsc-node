package devnet

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ContractDeployOpts holds options for deploying a WASM contract.
type ContractDeployOpts struct {
	// WasmPath is the host path to the compiled WASM bytecode file.
	WasmPath string
	// Name is the contract name.
	Name string
	// Description is an optional contract description.
	Description string
	// DeployerNode chooses which magi node becomes the contract's
	// `owner` (1-indexed, default 1). The contract is broadcast by
	// the dedicated `contract-deploy-1` Hive identity (so no magi
	// node has to be stopped), but the on-chain `contract.owner`
	// field is set to `<witnessPrefix><DeployerNode>` (e.g.
	// `magi.test1`) — preserving the prior behaviour that admin-
	// gated actions (checkAdmin) accept calls from CallContract(ctx,
	// DeployerNode, ...). Tests that historically used
	// DeployerNode=1 + CallContract(1, ...) for seedBlocks/etc keep
	// working unchanged.
	DeployerNode int
	// GQLNode is which magi node to request storage proof from
	// (1-indexed, default 1). The deployer talks to this node over
	// HTTP for the election lookup; libp2p connects to all nodes
	// via the pre-wired Bootnodes.
	GQLNode int
}

// DeployContract deploys a WASM contract to the running devnet.
//
// Uses the `contract-deploy-1` identity created by devnet-setup
// (cmd/devnet-setup/main.go:138-162). That identity:
//   - lives at <devnetDir>/contract-deploy-1/
//   - has its own libp2p identity (no conflict with any magi node)
//   - has explicit Bootnodes set for every magi-N (host+port+peerID)
//   - has Hive account `vsc-deployer-1` registered + funded with TBD
//
// So the deployer can join the libp2p mesh + pay the deploy fee
// without stopping any magi node. The old "stop magi-N + reuse its
// data-dir + restart" approach is gone — see audit memory for the
// fault trace.
func (d *Devnet) DeployContract(ctx context.Context, opts ContractDeployOpts) (string, error) {
	if !d.started {
		return "", fmt.Errorf("devnet not started")
	}
	if opts.WasmPath == "" {
		return "", fmt.Errorf("wasm path is required")
	}
	if opts.Name == "" {
		return "", fmt.Errorf("contract name is required")
	}
	if opts.DeployerNode == 0 {
		opts.DeployerNode = 1
	}
	if opts.GQLNode == 0 {
		opts.GQLNode = 1
	}
	// Map the 1-indexed DeployerNode to the matching witness Hive
	// account name. devnet-setup creates accounts as
	// `<witnessPrefix><N>` for N=1..cfg.Nodes (default prefix:
	// "magi.test"). Setting -owner=<that name> at deploy time keeps
	// admin-gated actions reachable by CallContract(ctx, N, ...).
	ownerAcct := fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, opts.DeployerNode)

	wasmPath, err := filepath.Abs(opts.WasmPath)
	if err != nil {
		return "", fmt.Errorf("resolving wasm path: %w", err)
	}
	if _, err := os.Stat(wasmPath); err != nil {
		return "", fmt.Errorf("wasm file not found: %w", err)
	}

	wasmDir := filepath.Dir(wasmPath)
	wasmFile := filepath.Base(wasmPath)

	log.Printf("[devnet] deploying contract %q (broadcast by vsc-deployer-1, owner=%s, GQL via magi-%d)...",
		opts.Name, ownerAcct, opts.GQLNode)

	deployCmd := []string{
		"./contract-deployer",
		"-network=devnet",
		"-data-dir=/data/devnet/contract-deploy-1",
		fmt.Sprintf("-wasmPath=/wasm/%s", wasmFile),
		fmt.Sprintf("-name=%s", opts.Name),
		fmt.Sprintf("-owner=%s", ownerAcct),
		fmt.Sprintf("-gqlUrl=http://magi-%d:8080/api/v1/graphql", opts.GQLNode),
	}
	if opts.Description != "" {
		deployCmd = append(deployCmd, fmt.Sprintf("-description=%s", opts.Description))
	}

	args := []string{
		"run", "--rm",
		"-v", fmt.Sprintf("%s:/wasm", wasmDir),
		"contract-deployer",
	}
	args = append(args, deployCmd...)

	out, err := d.composeOutput(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("contract deployment failed: %w\noutput: %s", err, out)
	}

	log.Printf("[devnet] contract-deployer output:\n%s", out)

	contractId := parseContractId(out)
	if contractId == "" {
		return "", fmt.Errorf("could not parse contract ID from output:\n%s", out)
	}

	log.Printf("[devnet] contract deployed: %s", contractId)
	return contractId, nil
}

// BuildCallTssContract builds the call-tss WASM contract using Docker
// TinyGo. Returns the path to the built .wasm file.
func BuildCallTssContract(ctx context.Context) (string, error) {
	contractDir := filepath.Join(findSourceRoot(), "tests", "devnet", "contracts", "call-tss")
	wasmPath := filepath.Join(contractDir, "bin", "build.wasm")

	log.Printf("[devnet] building call-tss contract...")
	cmd := exec.CommandContext(ctx, "make", "-C", contractDir, "build")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("building call-tss contract: %w", err)
	}

	if _, err := os.Stat(wasmPath); err != nil {
		return "", fmt.Errorf("built wasm not found at %s: %w", wasmPath, err)
	}

	log.Printf("[devnet] call-tss contract built: %s", wasmPath)
	return wasmPath, nil
}

// BuildBtcStubContract builds the btc-stub WASM contract used by oracle
// chain-relay devnet tests. The stub mimics the BTC mapping contract's
// `addBlocks` interface but skips all BTC validation, making it cheap to
// run inside a regtest devnet.
func BuildBtcStubContract(ctx context.Context) (string, error) {
	contractDir := filepath.Join(findSourceRoot(), "tests", "devnet", "contracts", "btc-stub")
	wasmPath := filepath.Join(contractDir, "bin", "build.wasm")

	log.Printf("[devnet] building btc-stub contract...")
	cmd := exec.CommandContext(ctx, "make", "-C", contractDir, "build")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("building btc-stub contract: %w", err)
	}

	if _, err := os.Stat(wasmPath); err != nil {
		return "", fmt.Errorf("built wasm not found at %s: %w", wasmPath, err)
	}

	log.Printf("[devnet] btc-stub contract built: %s", wasmPath)
	return wasmPath, nil
}

// TestContractPath returns the absolute path to the built-in test
// WASM contract at modules/e2e/artifacts/contract_test.wasm.
func TestContractPath() string {
	return filepath.Join(findSourceRoot(), "modules", "e2e", "artifacts", "contract_test.wasm")
}

// DashMappingContractPath returns the absolute path to the prebuilt
// dash-mapping-contract WASM used by the oracle Dash chain-relay devnet
// test. Unlike btc-stub, this is the REAL mapping contract that we expect
// to deploy on Magi testnet/mainnet.
//
// Resolution order:
//  1. DASH_MAPPING_WASM_PATH env var, if set
//  2. Sibling repo at <go-vsc-node>/../utxo-mapping/dash-mapping-contract/bin/testnet.wasm
//
// Returns an error (with a clear build hint) if the WASM cannot be
// located, so tests fail loudly instead of silently loading a stale or
// wrong-chain binary.
func DashMappingContractPath() (string, error) {
	candidates := make([]string, 0, 2)
	if p := os.Getenv("DASH_MAPPING_WASM_PATH"); p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates,
		filepath.Join(findSourceRoot(), "..", "utxo-mapping", "dash-mapping-contract", "bin", "testnet.wasm"),
	)

	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() && info.Size() > 0 {
			return abs, nil
		}
	}

	return "", fmt.Errorf(
		"dash-mapping-contract WASM not found. Tried: %v\n"+
			"Build it first:\n"+
			"  cd <utxo-mapping>/dash-mapping-contract && USE_DOCKER=1 make testnet\n"+
			"Or set DASH_MAPPING_WASM_PATH to an absolute path.",
		candidates,
	)
}

func parseContractId(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "contract id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "contract id:"))
		}
	}
	return ""
}
