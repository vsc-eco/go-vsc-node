package devnet

import (
	"context"
	"fmt"
	"log"
	"os"
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
	// DeployerNode is which magi node's identity to use (1-indexed, default 1).
	DeployerNode int
	// GQLNode is which magi node to request storage proof from (1-indexed, default 1).
	GQLNode int
}

// DeployContract deploys a WASM contract to the running devnet.
// It uses the contract-deployer Docker service with the specified
// node's identity for signing the Hive transaction.
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

	wasmPath, err := filepath.Abs(opts.WasmPath)
	if err != nil {
		return "", fmt.Errorf("resolving wasm path: %w", err)
	}
	if _, err := os.Stat(wasmPath); err != nil {
		return "", fmt.Errorf("wasm file not found: %w", err)
	}

	wasmDir := filepath.Dir(wasmPath)
	wasmFile := filepath.Base(wasmPath)

	log.Printf("[devnet] deploying contract %q from %s (node %d)...",
		opts.Name, wasmPath, opts.DeployerNode)

	// Build the deployer command that will run inside the container.
	deployCmd := []string{
		"./contract-deployer",
		"-network=devnet",
		fmt.Sprintf("-data-dir=/data/devnet/data-%d", opts.DeployerNode),
		fmt.Sprintf("-wasmPath=/wasm/%s", wasmFile),
		fmt.Sprintf("-name=%s", opts.Name),
		fmt.Sprintf("-gqlUrl=http://magi-%d:8080/api/v1/graphql", opts.GQLNode),
	}
	if opts.Description != "" {
		deployCmd = append(deployCmd, fmt.Sprintf("-description=%s", opts.Description))
	}

	// docker compose run --rm -v <wasmDir>:/wasm contract-deployer <cmd...>
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

// TestContractPath returns the absolute path to the built-in test
// WASM contract at modules/e2e/artifacts/contract_test.wasm.
func TestContractPath() string {
	return filepath.Join(findSourceRoot(), "modules", "e2e", "artifacts", "contract_test.wasm")
}

// parseContractId extracts the contract ID from contract-deployer output.
// The deployer prints "contract id: vsc1..." on success.
func parseContractId(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "contract id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "contract id:"))
		}
	}
	return ""
}
